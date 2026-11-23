//go:build linux

package netbind

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// Address mode (what happens without CAP_NET_RAW on old kernels), forced:
// the uplink still works, a destination routed elsewhere is refused.
func TestLiveAddrMode(t *testing.T) {
	live(t)
	u := uplink(t)
	bi, err := ForInterface(u.Name)
	if err != nil {
		t.Fatal(err)
	}
	b := bi.(*binder)
	b.os.mode = modeAddr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, _ := b.sourceAddr(netip.MustParseAddr("1.1.1.1"))
	oif, err := routeOif(netip.MustParseAddr("1.1.1.1"), src)
	if err != nil || oif != u.Index {
		t.Fatalf("routeOif %d %v", oif, err)
	}
	c, err := b.DialContext(ctx, "tcp", "1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("addr-mode dial from %v", c.LocalAddr())
	c.Close()
	if _, err := b.Ping(ctx, netip.MustParseAddr("1.1.1.1"), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	if c, err := b.DialContext(ctx, "tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("addr mode let a loopback destination through")
	} else {
		t.Logf("refused: %v", err)
	}
}
