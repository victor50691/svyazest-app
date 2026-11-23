package checks

// A netbind.Binder over the plain OS network (no interface pinning), for
// tests on the build server. The real one is agent "net"'s.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"svyazest-agent/internal/netbind"
)

type testBinder struct {
	iface   netbind.Iface
	sockopt map[string]any
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error) // nil = system
}

func newTestBinder(sockopt map[string]any) *testBinder {
	name := defaultIface()
	b := &testBinder{sockopt: sockopt, iface: netbind.Iface{Name: name, Up: true, Kind: "modem"}}
	if ifc, err := net.InterfaceByName(name); err == nil {
		b.iface.Index = ifc.Index
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				b.iface.Addrs = append(b.iface.Addrs, p.Addr())
			}
		}
	}
	return b
}

func defaultIface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "eth0"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 2 && fs[1] == "00000000" {
			return fs[0]
		}
	}
	return "eth0"
}

func (b *testBinder) Iface() netbind.Iface { return b.iface }

func (b *testBinder) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (b *testBinder) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, network, addr)
}

func (b *testBinder) LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if b.lookup != nil {
		return b.lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (b *testBinder) DNSInfo() ([]netip.Addr, string) {
	var out []netip.Addr
	data, _ := os.ReadFile("/etc/resolv.conf")
	for _, l := range strings.Split(string(data), "\n") {
		fs := strings.Fields(l)
		if len(fs) >= 2 && fs[0] == "nameserver" {
			if a, err := netip.ParseAddr(fs[1]); err == nil {
				out = append(out, a)
			}
		}
	}
	return out, "system"
}

func (b *testBinder) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// Ping: raw ICMP / ICMPv6 (the build server runs tests as root).
func (b *testBinder) Ping(ctx context.Context, ip netip.Addr, timeout time.Duration) (time.Duration, error) {
	ip = ip.Unmap()
	v6 := !ip.Is4()
	proto, laddr, reqType, replyType := "ip4:icmp", "0.0.0.0", byte(8), byte(0)
	if v6 {
		proto, laddr, reqType, replyType = "ip6:ipv6-icmp", "::", 128, 129
	}
	c, err := net.ListenPacket(proto, laddr)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	id := uint16(os.Getpid() & 0xffff)
	seq := uint16(time.Now().UnixNano() & 0xffff)
	msg := []byte{reqType, 0, 0, 0, byte(id >> 8), byte(id), byte(seq >> 8), byte(seq), 's', 'v', 'z', '!'}
	if !v6 { // the kernel fills in the ICMPv6 checksum
		cs := icmpChecksum(msg)
		msg[2], msg[3] = byte(cs>>8), byte(cs)
	}
	start := time.Now()
	dl := start.Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	c.SetDeadline(dl)
	if _, err := c.WriteTo(msg, &net.IPAddr{IP: net.IP(ip.AsSlice())}); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		ia, _ := from.(*net.IPAddr)
		if ia == nil || !ia.IP.Equal(net.IP(ip.AsSlice())) || n < 8 {
			continue
		}
		if buf[0] == replyType && binary.BigEndian.Uint16(buf[4:]) == id && binary.BigEndian.Uint16(buf[6:]) == seq {
			return time.Since(start), nil
		}
	}
}

func icmpChecksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func (b *testBinder) XraySockopt() map[string]any {
	out := map[string]any{}
	for k, v := range b.sockopt {
		out[k] = v
	}
	return out
}

func (b *testBinder) Counters() (uint64, uint64, error) {
	rd := func(n string) (uint64, error) {
		d, err := os.ReadFile("/sys/class/net/" + b.iface.Name + "/statistics/" + n)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(d)), 10, 64)
	}
	rx, err := rd("rx_bytes")
	if err != nil {
		return 0, 0, err
	}
	tx, err := rd("tx_bytes")
	return rx, tx, err
}

var errNoDNS = errors.New("dns dead")
