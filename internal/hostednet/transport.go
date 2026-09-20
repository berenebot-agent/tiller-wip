// Package hostednet contains the outbound network policy used by hosted Tiller.
// Local mode intentionally does not use this package so LAN provider support is
// preserved.
package hostednet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRedirects = 3

var blockedPrefixes = []netip.Prefix{
	// IPv4 special-use and private ranges.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// Common cloud metadata endpoints. The link-local range above catches the
	// standard address; this explicit entry documents the security boundary.
	netip.MustParsePrefix("100.100.100.200/32"),
	// IPv6 special-use and private ranges.
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("ff00::/8"),
}

var blockedHostSuffixes = []string{
	".internal",
	".localhost",
	".local",
	".lan",
	".home.arpa",
}

// Resolver is the DNS operation needed by Transport. It is an interface so
// the DNS-rebinding and mixed-address cases can be tested without live DNS.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// Transport validates every request and dials only addresses that pass the
// hosted public-network policy. The wrapped http.Transport retains normal TLS
// hostname verification and connection pooling.
type Transport struct {
	transport *http.Transport
	resolver  Resolver
	dialer    dialContextFunc
}

// NewTransport returns the production hosted outbound transport. Proxy
// environment variables are deliberately ignored: user-controlled requests
// must not be routed through an unreviewed proxy that can bypass this policy.
func NewTransport() *Transport {
	return newTransport(net.DefaultResolver, (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)
}

func newTransport(resolver Resolver, dialer dialContextFunc) *Transport {
	t := &Transport{resolver: resolver, dialer: dialer}
	t.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           t.dialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}
	return t
}

// NewClient returns an HTTP client using the hosted transport and the shared
// redirect policy.
func NewClient(timeout time.Duration) *http.Client {
	t := NewTransport()
	return &http.Client{Transport: t, CheckRedirect: t.CheckRedirect, Timeout: timeout}
}

// RoundTrip rejects non-hosted destinations before any network operation.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("hosted outbound request is nil")
	}
	if err := ValidateURL(req.URL); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req)
}

// CheckRedirect applies the same URL policy to every redirect and limits the
// chain to three followed redirects. Dial-time validation remains authoritative
// because the destination can change between redirect handling and connection.
func (t *Transport) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return http.ErrUseLastResponse
	}
	return ValidateURL(req.URL)
}

// CloneWithResponseHeaderTimeout returns an independent transport for an
// account-specific upstream header timeout.
func (t *Transport) CloneWithResponseHeaderTimeout(timeout time.Duration) *Transport {
	clone := &Transport{resolver: t.resolver, dialer: t.dialer}
	clone.transport = t.transport.Clone()
	clone.transport.ResponseHeaderTimeout = timeout
	clone.transport.DialContext = clone.dialContext
	return clone
}

func (t *Transport) ResponseHeaderTimeout() time.Duration {
	return t.transport.ResponseHeaderTimeout
}

func (t *Transport) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
}

func (t *Transport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("hosted outbound address is invalid: %w", err)
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("hosted outbound network %q is not permitted", network)
	}
	if parsedPort, err := strconv.Atoi(port); err != nil || parsedPort < 1 || parsedPort > 65535 {
		return nil, errors.New("hosted outbound port is invalid")
	}

	addresses, err := t.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("hosted outbound DNS lookup failed: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("hosted outbound DNS lookup returned no addresses")
	}
	for _, address := range addresses {
		if !AllowedAddress(address) {
			return nil, errors.New("hosted outbound destination resolved to a private or reserved address")
		}
	}

	var lastErr error
	for _, address := range addresses {
		if network == "tcp4" && !address.Unmap().Is4() {
			continue
		}
		if network == "tcp6" && address.Unmap().Is4() {
			continue
		}
		// Dial the validated IP directly. This closes the DNS check/use gap:
		// the socket never asks the resolver to reinterpret the hostname.
		conn, dialErr := t.dialer(ctx, network, net.JoinHostPort(address.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("hosted outbound DNS result has no address for the requested network")
}

func (t *Transport) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if isBlockedHostname(host) {
		return nil, errors.New("hosted outbound hostname is internal-only")
	}
	return t.resolver.LookupNetIP(ctx, "ip", host)
}

// Validate parses the URL and applies the non-DNS portion of the hosted
// destination policy. DNS and actual-dial validation occur in Transport.
func Validate(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return ValidateURL(u)
}

func ValidateURL(u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return errors.New("hosted outbound URL must use HTTPS")
	}
	if u.User != nil || u.Fragment != "" || u.Hostname() == "" {
		return errors.New("hosted outbound URL has forbidden userinfo, fragment, or hostname")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("hosted outbound URL must use port 443")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if isBlockedHostname(host) {
		return errors.New("hosted outbound hostname is internal-only")
	}
	if address, err := netip.ParseAddr(host); err == nil && !AllowedAddress(address) {
		return errors.New("hosted outbound URL resolves to a private or reserved address")
	}
	return nil
}

// AllowedAddress reports whether an address is suitable for a hosted public
// outbound connection. All addresses returned for a hostname must pass this
// check; a single private answer rejects the request.
func AllowedAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func isBlockedHostname(host string) bool {
	if host == "localhost" || host == "host.docker.internal" || host == "gateway.docker.internal" {
		return true
	}
	for _, suffix := range blockedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
