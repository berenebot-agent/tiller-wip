// Package dnsfallback adds ordered failover to Go's native DNS exchanges.
// The native resolver still handles hosts files, search domains, DNS record
// parsing, TCP fallback, and address selection. Successful and negative DNS
// answers remain authoritative; only failed exchanges use another server.
package dnsfallback

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// DefaultServers tries both Cloudflare addresses before Google's addresses.
const DefaultServers = "1.1.1.1,1.0.0.1,8.8.8.8,8.8.4.4"

// ParseServers accepts comma-separated IP addresses, optionally with ports.
// Empty selects the defaults; "off" disables failover entirely. Hostnames are
// deliberately disallowed: resolving a DNS server must not require working DNS.
func ParseServers(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "off") {
		return nil, nil
	}
	if raw == "" {
		raw = DefaultServers
	}
	var servers []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		var address netip.AddrPort
		if ip, err := netip.ParseAddr(value); err == nil {
			address = netip.AddrPortFrom(ip, 53)
		} else {
			address, err = netip.ParseAddrPort(value)
			if err != nil || address.Port() == 0 {
				return nil, fmt.Errorf("DNS fallback server must be an IP address or IP:port, got %q", value)
			}
		}
		servers = appendUnique(servers, address.String())
	}
	return servers, nil
}

// New returns a resolver suitable for installation as net.DefaultResolver,
// before starting any network clients or goroutines. This covers providers,
// discovery, notifications, OAuth, and SMTP without changing their transports
// or hosted destination-validation rules. A nil list leaves native DNS intact.
func New(servers []string) *net.Resolver {
	return newResolver(servers, systemServers, time.Second)
}

func newResolver(fallbacks []string, supplied func(string) []string, attemptTimeout time.Duration) *net.Resolver {
	resolver := &net.Resolver{}
	if len(fallbacks) == 0 {
		return resolver
	}
	fallbacks = append([]string(nil), fallbacks...)
	resolver.PreferGo = true
	resolver.Dial = func(ctx context.Context, network, primary string) (net.Conn, error) {
		servers := supplied(primary)
		for _, server := range fallbacks {
			servers = appendUnique(servers, server)
		}
		return newExchangeConn(ctx, network, servers, attemptTimeout), nil
	}
	return resolver
}

// Try every supplied nameserver before public fallbacks. Go passes the server
// selected from the current OS config to Dial; retain that selection first,
// then read the remaining nameservers. Do not freeze host DNS at startup.
func systemServers(primary string) []string {
	servers := []string{primary}
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return servers
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if ip, err := netip.ParseAddr(fields[1]); err == nil {
			servers = appendUnique(servers, netip.AddrPortFrom(ip, 53).String())
		}
	}
	return servers
}

func appendUnique(servers []string, server string) []string {
	for _, existing := range servers {
		if existing == server {
			return servers
		}
	}
	return append(servers, server)
}

type exchangeConn struct {
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	result <-chan error
}

// A stream connection lets the native resolver frame its query and validate
// the returned DNS message. The worker uses the requested UDP/TCP network on
// the wire; a truncated UDP reply is returned unchanged so Go retries with TCP.
func newExchangeConn(ctx context.Context, network string, servers []string, timeout time.Duration) net.Conn {
	ctx, cancel := context.WithCancel(ctx)
	client, worker := net.Pipe()
	result := make(chan error, 1)
	go func() {
		stop := context.AfterFunc(ctx, func() { worker.Close() })
		defer stop()
		defer worker.Close()
		defer cancel()
		query, err := readFrame(worker)
		if err == nil {
			var response []byte
			response, err = exchange(ctx, network, servers, query, timeout)
			if err == nil {
				err = writeFrame(worker, response)
			}
		}
		result <- err
	}()
	return &exchangeConn{Conn: client, ctx: ctx, cancel: cancel, result: result}
}

func (c *exchangeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		select {
		case cause := <-c.result:
			if cause != nil {
				return n, cause
			}
		default:
		}
		if cause := c.ctx.Err(); cause != nil {
			return n, cause
		}
	}
	return n, err
}

func (c *exchangeConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func exchange(ctx context.Context, network string, servers []string, query []byte, timeout time.Duration) ([]byte, error) {
	var lastResponse []byte
	var lastErr error
	for i, server := range servers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		budget := timeout
		if deadline, ok := ctx.Deadline(); ok {
			// Leave time for later candidates when supplied DNS silently drops
			// packets. Never extend the native resolver/request deadline.
			remaining := time.Until(deadline) / time.Duration(len(servers)-i)
			if remaining < budget {
				budget = remaining
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		response, err := roundTrip(attemptCtx, network, server, query)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		// RCODE 0 (success, including NODATA) and 3 (NXDOMAIN) are
		// authoritative. A truncated response belongs to Go's TCP retry,
		// not another resolver. SERVFAIL/REFUSED/etc. permit failover.
		rcode := response[3] & 0x0f
		if rcode == 0 || rcode == 3 || response[2]&0x02 != 0 {
			return response, nil
		}
		lastResponse = response
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS servers available")
	}
	return nil, lastErr
}

func roundTrip(ctx context.Context, network, server string, query []byte) ([]byte, error) {
	if len(query) < 12 {
		return nil, errors.New("invalid DNS query")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(network, "tcp") {
		if err := writeFrame(conn, query); err != nil {
			return nil, err
		}
		response, err := readFrame(conn)
		if err != nil {
			return nil, err
		}
		if !matchingResponse(query, response) {
			return nil, errors.New("invalid DNS response")
		}
		return response, nil
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	response := make([]byte, 65535)
	for {
		n, err := conn.Read(response)
		if err != nil {
			return nil, err
		}
		// Ignore unrelated/malformed datagrams until the attempt deadline.
		// Full question and record validation is still done by net.Resolver.
		if matchingResponse(query, response[:n]) {
			return response[:n], nil
		}
	}
}

func matchingResponse(query, response []byte) bool {
	return len(response) >= 12 && response[2]&0x80 != 0 && response[0] == query[0] && response[1] == query[1]
}

func readFrame(reader io.Reader) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return nil, err
	}
	message := make([]byte, int(binary.BigEndian.Uint16(size[:])))
	_, err := io.ReadFull(reader, message)
	return message, err
}

func writeFrame(writer io.Writer, message []byte) error {
	frame := make([]byte, 2+len(message))
	binary.BigEndian.PutUint16(frame, uint16(len(message)))
	copy(frame[2:], message)
	n, err := writer.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}
