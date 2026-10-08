package dnsfallback

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/hostednet"
)

// Mock DNS uses local UDP and TCP sockets, including real wire framing. No
// test calls a public resolver or provider.
type mockDNS struct {
	address string
	udp     atomic.Int32
	tcp     atomic.Int32
}

func startDNS(t *testing.T, handler func([]byte, string) []byte) *mockDNS {
	t.Helper()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", udp.LocalAddr().String())
	if err != nil {
		udp.Close()
		t.Fatal(err)
	}
	mock := &mockDNS{address: udp.LocalAddr().String()}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		buffer := make([]byte, 65535)
		for {
			n, address, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			mock.udp.Add(1)
			if reply := handler(buffer[:n], "udp"); reply != nil {
				udp.WriteTo(reply, address)
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			query, err := readFrame(conn)
			if err == nil {
				mock.tcp.Add(1)
				if reply := handler(query, "tcp"); reply != nil {
					writeFrame(conn, reply)
				}
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		udp.Close()
		tcp.Close()
		workers.Wait()
	})
	return mock
}

func answer(query []byte, rcode byte, ip string) []byte {
	// Keep exactly the question; the native query also contains EDNS options.
	end := 12
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}
	end += 5
	response := append([]byte(nil), query[:end]...)
	response[2], response[3] = 0x81, 0x80|rcode
	for i := 6; i < 12; i++ {
		response[i] = 0
	}
	if rcode != 0 || ip == "" {
		return response
	}
	qtype := binary.BigEndian.Uint16(query[end-4 : end-2])
	address := netip.MustParseAddr(ip)
	if qtype == 1 && address.Is4() || qtype == 28 && address.Is6() {
		response[7] = 1
		data := address.AsSlice()
		response = append(response, 0xc0, 0x0c, byte(qtype>>8), byte(qtype), 0, 1, 0, 0, 0, 30, 0, byte(len(data)))
		response = append(response, data...)
	}
	return response
}

func mockResolver(supplied []*mockDNS, fallbacks []*mockDNS, timeout time.Duration) *net.Resolver {
	var public []string
	for _, server := range fallbacks {
		public = append(public, server.address)
	}
	return newResolver(public, func(string) []string {
		var servers []string
		for _, server := range supplied {
			servers = append(servers, server.address)
		}
		return servers
	}, timeout)
}

func lookup(t *testing.T, resolver *net.Resolver, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addresses, err := resolver.LookupNetIP(ctx, "ip4", "provider.example.")
	if err != nil || len(addresses) != 1 || addresses[0].String() != want {
		t.Fatalf("lookup = %v, %v; want %s", addresses, err, want)
	}
}

func TestSuppliedDNSRemainsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rcode byte
		ip    string
	}{
		{"private answer", 0, "192.168.1.10"},
		{"NXDOMAIN", 3, ""},
		{"NODATA", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, tc.rcode, tc.ip) })
			fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
			resolver := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond)
			if tc.ip != "" {
				lookup(t, resolver, tc.ip)
			} else {
				_, err := resolver.LookupNetIP(context.Background(), "ip4", "provider.example.")
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Fatalf("negative reply = %v, want not-found", err)
				}
			}
			if fallback.udp.Load()+fallback.tcp.Load() != 0 {
				t.Fatal("authoritative response used public fallback")
			}
		})
	}
}

func TestAllSuppliedServersBeforeFallback(t *testing.T) {
	first := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	second := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "10.1.2.3") })
	public := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	lookup(t, mockResolver([]*mockDNS{first, second}, []*mockDNS{public}, 100*time.Millisecond), "10.1.2.3")
	if first.udp.Load() != 1 || second.udp.Load() != 1 || public.udp.Load() != 0 {
		t.Fatal("did not exhaust supplied DNS before public fallback")
	}
}

func TestCloudflareThenGoogleAfterSuppliedFailure(t *testing.T) {
	for _, failure := range []string{"SERVFAIL", "REFUSED", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			server := func(name string, rcode byte, ip string, drop bool) *mockDNS {
				return startDNS(t, func(q []byte, _ string) []byte {
					mu.Lock()
					order = append(order, name)
					mu.Unlock()
					if drop {
						return nil
					}
					return answer(q, rcode, ip)
				})
			}
			rcode := byte(2)
			if failure == "REFUSED" {
				rcode = 5
			}
			primary := server("supplied", rcode, "", failure == "timeout")
			cloudflare1 := server("cloudflare1", rcode, "", failure == "timeout")
			cloudflare2 := server("cloudflare2", rcode, "", failure == "timeout")
			google1 := server("google1", 0, "93.184.216.34", false)
			google2 := server("google2", 0, "93.184.216.35", false)
			lookup(t, mockResolver([]*mockDNS{primary}, []*mockDNS{cloudflare1, cloudflare2, google1, google2}, 30*time.Millisecond), "93.184.216.34")
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(order, []string{"supplied", "cloudflare1", "cloudflare2", "google1"}) {
				t.Fatalf("DNS order = %v", order)
			}
		})
	}
}

func TestFallbackNegativeReplyStopsChain(t *testing.T) {
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	cloudflare := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 3, "") })
	google := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	_, err := mockResolver([]*mockDNS{primary}, []*mockDNS{cloudflare, google}, 100*time.Millisecond).LookupNetIP(context.Background(), "ip4", "provider.example.")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound || google.udp.Load() != 0 {
		t.Fatalf("negative fallback = %v, Google queries = %d", err, google.udp.Load())
	}
}

func TestRecoveredSuppliedDNSIsUsedAgain(t *testing.T) {
	var recovered atomic.Bool
	primary := startDNS(t, func(q []byte, _ string) []byte {
		if recovered.Load() {
			return answer(q, 0, "192.168.1.10")
		}
		return answer(q, 2, "")
	})
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	resolver := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond)
	lookup(t, resolver, "93.184.216.34")
	recovered.Store(true)
	lookup(t, resolver, "192.168.1.10")
	if fallback.udp.Load() != 1 {
		t.Fatal("continued using fallback after supplied DNS recovered")
	}
}

func TestTruncatedUDPUsesTCP(t *testing.T) {
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	fallback := startDNS(t, func(q []byte, network string) []byte {
		reply := answer(q, 0, "93.184.216.34")
		if network == "udp" {
			reply[2] |= 0x02
		}
		return reply
	})
	lookup(t, mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond), "93.184.216.34")
	if fallback.udp.Load() != 1 || fallback.tcp.Load() != 1 {
		t.Fatalf("fallback queries UDP=%d TCP=%d", fallback.udp.Load(), fallback.tcp.Load())
	}
}

func TestCancellationStopsFailover(t *testing.T) {
	queried := make(chan struct{}, 1)
	primary := startDNS(t, func([]byte, string) []byte {
		select {
		case queried <- struct{}{}:
		default:
		}
		return nil
	})
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	resolver := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := resolver.LookupNetIP(ctx, "ip4", "provider.example.")
		done <- err
	}()
	select {
	case <-queried:
	case <-time.After(time.Second):
		t.Fatal("primary DNS not queried")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled lookup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled lookup did not terminate")
	}
	if fallback.udp.Load() != 0 {
		t.Fatal("cancelled lookup used public fallback")
	}
}

func TestHostsFileBypassesDNS(t *testing.T) {
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	resolver := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond)
	if _, err := resolver.LookupNetIP(context.Background(), "ip", "localhost"); err != nil {
		t.Fatal(err)
	}
	if primary.udp.Load()+fallback.udp.Load() != 0 {
		t.Fatal("localhost queried DNS")
	}
}

func TestNativeDialerUsesFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "127.0.0.1") })
	resolver := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond)
	transport := &http.Transport{DialContext: (&net.Dialer{Resolver: resolver}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("http://provider.example.:" + port)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTP status = %d", response.StatusCode)
	}
}

func TestHostedPolicyRejectsPrivateFallbackAnswer(t *testing.T) {
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "127.0.0.1") })
	previous := net.DefaultResolver
	net.DefaultResolver = mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 100*time.Millisecond)
	t.Cleanup(func() { net.DefaultResolver = previous })
	transport := hostednet.NewTransport()
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://provider.example/", nil)
	response, err := transport.RoundTrip(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "private or reserved") {
		t.Fatalf("hosted private fallback answer = %v", err)
	}
}

func TestParseServers(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", []string{"1.1.1.1:53", "1.0.0.1:53", "8.8.8.8:53", "8.8.4.4:53"}},
		{"off", nil},
		{" OFF ", nil},
		{"10.1.1.1, [::1]:5353,10.1.1.1", []string{"10.1.1.1:53", "[::1]:5353"}},
		{"2606:4700:4700::1111", []string{"[2606:4700:4700::1111]:53"}},
	} {
		got, err := ParseServers(tc.raw)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseServers(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"dns.example", "1.1.1.1,", "1.1.1.1:0", "1.1.1.1:65536", "off,1.1.1.1"} {
		if _, err := ParseServers(raw); err == nil {
			t.Errorf("ParseServers(%q) succeeded", raw)
		}
	}
	if resolver := New(nil); resolver.Dial != nil || resolver.PreferGo {
		t.Fatal("disabled fallback changes native resolver")
	}
}

func TestAllServersFail(t *testing.T) {
	primary := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 2, "") })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := mockResolver([]*mockDNS{primary}, []*mockDNS{fallback}, 30*time.Millisecond).LookupNetIP(ctx, "ip4", "provider.example.")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsTemporary {
		t.Fatalf("all SERVFAIL = %v", err)
	}
}

func TestExchangeTransportFailureAndMalformedPacket(t *testing.T) {
	// An unreachable supplied DNS socket must not prevent the next resolver.
	closed, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.LocalAddr().String()
	closed.Close()
	fallback := startDNS(t, func(q []byte, _ string) []byte { return answer(q, 0, "93.184.216.34") })
	resolver := newResolver([]string{fallback.address}, func(string) []string { return []string{address} }, 30*time.Millisecond)
	lookup(t, resolver, "93.184.216.34")
	malformed := startDNS(t, func([]byte, string) []byte { return []byte{0, 0, 0} })
	lookup(t, mockResolver([]*mockDNS{malformed}, []*mockDNS{fallback}, 30*time.Millisecond), "93.184.216.34")
}

func TestDeadlineLeavesBudgetForLaterResolvers(t *testing.T) {
	var servers []*mockDNS
	for i := 0; i < 5; i++ {
		servers = append(servers, startDNS(t, func([]byte, string) []byte { return nil }))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	// IP lookup singleflight strips the caller's deadline while retaining
	// cancellation. Test the native DNS-exchange deadline passed to Dial
	// directly; caller cancellation is covered separately above.
	resolver := mockResolver(servers[:1], servers[1:], time.Second)
	conn, err := resolver.Dial(ctx, "udp", "127.0.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	query := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 8, 'p', 'r', 'o', 'v', 'i', 'd', 'e', 'r', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 0, 1, 0, 1}
	if err = writeFrame(conn, query); err == nil {
		_, err = readFrame(conn)
	}
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("timed-out lookup = %v, elapsed = %v", err, time.Since(started))
	}
	for i, server := range servers {
		if server.udp.Load() == 0 {
			t.Errorf("deadline exhausted before resolver %d was attempted", i)
		}
	}
}
