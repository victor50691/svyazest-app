package netbind

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests touch the real network of the build box: NETBIND_LIVE=1.
func live(t *testing.T) {
	if os.Getenv("NETBIND_LIVE") == "" {
		t.Skip("NETBIND_LIVE not set")
	}
}

// uplink: the interface with a default route (the build box has one).
func uplink(t *testing.T) IfaceDetail {
	l, err := ListDetailed()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range l {
		t.Logf("%-12s idx=%d kind=%-8s up=%v gw=%v def=%v drv=%q dns=%v addrs=%v", d.Name, d.Index, d.Kind, d.Up, d.Gateway, d.DefaultRoute, d.Driver, d.DNS, d.Addrs)
	}
	for _, d := range l {
		if d.Up && d.DefaultRoute && d.Kind != "virtual" {
			return d
		}
	}
	t.Fatal("no uplink")
	return IfaceDetail{}
}

func TestLiveBinder(t *testing.T) {
	live(t)
	u := uplink(t)
	b, err := ForInterface(u.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("warning: %q", BindWarning(b))
	srv, src := b.DNSInfo()
	t.Logf("dns %v (%s)", srv, src)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ips, err := b.LookupHost(ctx, "example.com")
	if err != nil || len(ips) == 0 {
		t.Fatalf("LookupHost: %v %v", ips, err)
	}
	t.Logf("example.com -> %v", ips)
	if _, err := b.LookupHost(ctx, "no-such-host-svyazest.invalid"); err == nil {
		t.Fatal("expected NXDOMAIN")
	}

	c, err := b.DialContext(ctx, "tcp", "1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dial local %v", c.LocalAddr())
	c.Close()

	resp, err := b.HTTPClient(15 * time.Second).Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ip=") {
		t.Fatalf("trace: %s", body)
	}

	pc, err := b.ListenPacket(ctx, "udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("listen %v", pc.LocalAddr())
	pc.Close()

	rtt, err := b.Ping(ctx, netip.MustParseAddr("1.1.1.1"), 3*time.Second)
	if err != nil {
		t.Fatal("ping:", err)
	}
	t.Logf("ping 1.1.1.1 %v", rtt)
	if _, err := b.Ping(ctx, netip.MustParseAddr("192.0.2.1"), 1500*time.Millisecond); err == nil {
		t.Fatal("TEST-NET ping must not answer")
	}
	if hasGlobal6(u.Addrs) {
		c6, err := b.DialContext(ctx, "tcp6", "[2606:4700:4700::1111]:443")
		if err != nil {
			t.Fatal("v6 dial:", err)
		}
		c6.Close()
		if rtt, err := b.Ping(ctx, netip.MustParseAddr("2606:4700:4700::1111"), 3*time.Second); err != nil {
			t.Fatal("v6 ping:", err)
		} else {
			t.Logf("ping6 %v", rtt)
		}
	}
	rx, tx, err := b.Counters()
	if err != nil || rx == 0 || tx == 0 {
		t.Fatalf("counters %d %d %v", rx, tx, err)
	}
	t.Logf("sockopt %v", b.XraySockopt())

	// Bound to the uplink, loopback is unreachable: SO_BINDTODEVICE works.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	dctx, dc := context.WithTimeout(ctx, 2*time.Second)
	defer dc()
	if c, err := b.DialContext(dctx, "tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("bound dial reached 127.0.0.1 -- not pinned")
	} else {
		t.Logf("bound dial to loopback refused as expected: %v", err)
	}
}

// Control: the API on 127.0.0.1 is not reachable through the bound uplink
// route, so the request must fall over to "default" and keep the body.
func TestLiveControlFallback(t *testing.T) {
	live(t)
	u := uplink(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte(r.Header.Get("X-Net-Route") + "|" + string(b)))
	}))
	defer srv.Close()
	c := NewControl("")
	req, _ := http.NewRequest("POST", srv.URL+"/x", io.NopCloser(strings.NewReader("hello")))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "default|hello" || c.LastRoute() != "default" {
		t.Fatalf("got %q route %q", b, c.LastRoute())
	}
	// The uplink route failed -> it is now behind "default".
	rs := c.(*controlImpl).routes()
	if rs[0].id != "default" {
		t.Fatalf("order after failure: %v", rs)
	}
	t.Logf("routes after failure: %v (uplink %s)", rs, u.Name)

	// Real internet: through the uplink route.
	c2 := NewControl("")
	req2, _ := http.NewRequest("GET", "https://www.cloudflare.com/cdn-cgi/trace", nil)
	resp2, err := c2.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if c2.LastRoute() != u.Kind+":"+u.Name {
		t.Fatalf("route %q", c2.LastRoute())
	}
	// With the uplink declared as the modem it becomes "cellular", last.
	c3 := NewControl(u.Name)
	rs3 := c3.(*controlImpl).routes()
	if len(rs3) != 2 || rs3[0].id != "default" || rs3[1].id != "cellular" {
		t.Fatalf("modem order: %v", rs3)
	}
}
