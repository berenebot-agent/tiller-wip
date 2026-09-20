package hostednet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestValidateHostedURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.com",
		"https://example.com:8443/path",
		"https://user:pass@example.com/path",
		"https://127.0.0.1/path",
		"https://localhost/path",
		"https://host.docker.internal/path",
		"https://example.internal/path",
	} {
		if err := Validate(raw); err == nil {
			t.Errorf("Validate(%q) succeeded", raw)
		}
	}
	for _, raw := range []string{"https://example.com", "https://example.com/v1?tenant=one"} {
		if err := Validate(raw); err != nil {
			t.Errorf("Validate(%q) = %v", raw, err)
		}
	}
}

func TestAllowedAddressRejectsSpecialUseRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1",
		"169.254.169.254", "172.16.0.1", "192.168.1.1", "224.0.0.1",
		"240.0.0.1", "::1", "fc00::1", "fe80::1", "ff02::1",
	} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(err)
		}
		if AllowedAddress(address) {
			t.Errorf("AllowedAddress(%s) = true", raw)
		}
	}
	public := netip.MustParseAddr("93.184.216.34")
	if !AllowedAddress(public) {
		t.Fatalf("AllowedAddress(%s) = false", public)
	}
}

func TestTransportRejectsAnyPrivateDNSAnswer(t *testing.T) {
	transport := newTransport(resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("192.168.1.10"),
		}, nil
	}), func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialer called for a rejected DNS result")
		return nil, nil
	})
	_, err := transport.dialContext(context.Background(), "tcp", "models.example.com:443")
	if err == nil {
		t.Fatal("dialContext accepted a mixed public/private DNS result")
	}
}

func TestTransportRevalidatesAddressAtDial(t *testing.T) {
	stop := errors.New("stop after dial validation")
	var dialed string
	transport := newTransport(resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}), func(_ context.Context, network, address string) (net.Conn, error) {
		dialed = network + " " + address
		return nil, stop
	})
	_, err := transport.dialContext(context.Background(), "tcp", "models.example.com:443")
	if !errors.Is(err, stop) {
		t.Fatalf("dialContext error = %v, want dialer error", err)
	}
	if dialed != "tcp 93.184.216.34:443" {
		t.Fatalf("dialed %q, want validated address", dialed)
	}
}

func TestTransportRejectsInvalidRoundTripURL(t *testing.T) {
	transport := NewTransport()
	request, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip accepted a non-HTTPS URL")
	}
}

func TestCheckRedirectRevalidatesAndBoundsChain(t *testing.T) {
	transport := NewTransport()
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.CheckRedirect(request, make([]*http.Request, maxRedirects)); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect limit error = %v", err)
	}
	request.URL.Scheme = "http"
	if err := transport.CheckRedirect(request, nil); err == nil {
		t.Fatal("CheckRedirect accepted a non-HTTPS redirect")
	}
}
