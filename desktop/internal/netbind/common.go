package netbind

// OS-independent half of the Binder: name resolution through the
// interface's own DNS, dialing a hostname IP by IP, the HTTP client and the
// interface snapshot. The OS files (netbind_linux.go, netbind_windows.go)
// supply how a socket is pinned to the interface (b.control, b.localAddr),
// Ping, Counters and the interface list.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// IfaceDetail is Iface plus what List learned on the way. Additive to the
// contract: the modem package needs the driver / adapter description to
// tell a USB-tethered phone from a HiLink stick, the control channel needs
// to know whether a default route leaves through the interface even when
// it has no gateway address (point-to-point wwan/ppp: "default dev wwan0").
type IfaceDetail struct {
	Iface
	Driver       string // Linux: kernel driver of the device ("qmi_wwan", "rndis_host"); "" for virtual
	Description  string // Windows: adapter description; Linux: USB "manufacturer product" if known
	DefaultRoute bool   // a default route (v4 or v6) goes out of this interface
}

// ListDetailed is List with the extra fields above.
func ListDetailed() ([]IfaceDetail, error) { return listDetailedImpl(true) }

func listImpl() ([]Iface, error) {
	d, err := listDetailedImpl(true)
	if err != nil {
		return nil, err
	}
	out := make([]Iface, len(d))
	for i := range d {
		out[i] = d[i].Iface
	}
	return out, nil
}

// BindWarning: "" when the binder pins sockets to the interface for real
// (SO_BINDTODEVICE on Linux, IP_UNICAST_IF on Windows). Otherwise a text
// for the UI/log: on Linux without CAP_NET_RAW (kernels < 5.7) the binder
// only binds the source address and refuses destinations the kernel would
// route elsewhere -- checks still never leak, but may fail.
func BindWarning(b Binder) string {
	if x, ok := b.(*binder); ok {
		return x.warn
	}
	return ""
}

// Public resolvers used over the interface when it has no DNS of its own
// (e.g. a modem whose DHCP lease we could not find). Yandex first: under
// «белые списки» it is the one most likely to stay reachable on mobile.
var publicDNS4 = []netip.Addr{
	netip.MustParseAddr("77.88.8.8"),
	netip.MustParseAddr("1.1.1.1"),
	netip.MustParseAddr("8.8.8.8"),
}
var publicDNS6 = []netip.Addr{
	netip.MustParseAddr("2a02:6b8::feed:0ff"),
	netip.MustParseAddr("2606:4700:4700::1111"),
}

const perServerDNSTimeout = 3 * time.Second

// binder: one interface. The OS-specific fields live in osBinder
// (defined per OS) so this struct compiles everywhere.
type binder struct {
	ifc  Iface
	warn string
	os   osBinder

	dnsServers []netip.Addr
	dnsSource  string // "iface" | "public" (planned)

	mu         sync.Mutex
	lastSource string // what the last LookupHost actually used ("system" after fallback)
	addrsAt    time.Time
	addrs      []netip.Addr
}

func newBinder(d IfaceDetail) *binder {
	b := &binder{ifc: d.Iface, addrs: d.Addrs, addrsAt: time.Now()}
	if len(d.DNS) > 0 {
		b.dnsServers = append([]netip.Addr(nil), d.DNS...)
		b.dnsSource = "iface"
	} else {
		b.dnsServers = append([]netip.Addr(nil), publicDNS4...)
		if hasGlobal6(d.Addrs) {
			b.dnsServers = append(b.dnsServers, publicDNS6...)
		}
		b.dnsSource = "public"
	}
	return b
}

func (b *binder) Iface() Iface { return b.ifc }

func (b *binder) DNSInfo() ([]netip.Addr, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastSource == "system" {
		return nil, "system"
	}
	return append([]netip.Addr(nil), b.dnsServers...), b.dnsSource
}

func (b *binder) XraySockopt() map[string]any {
	// Both xray versions we ship (26.3.27 and 26.10.31) implement
	// sockopt.interface on Linux (SO_BINDTODEVICE) and on Windows
	// (net.InterfaceByName -> IP_UNICAST_IF / IPV6_UNICAST_IF), and on
	// Windows Go's interface name is the adapter's FriendlyName -- the same
	// string we use as Iface.Name.
	return map[string]any{"interface": b.ifc.Name}
}

// currentAddrs: the interface's addresses now (a modem may reconnect with a
// new IP while the binder lives); cached for 3 s because every dial asks.
func (b *binder) currentAddrs() []netip.Addr {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.addrsAt) < 3*time.Second {
		return b.addrs
	}
	if ifi, err := net.InterfaceByName(b.ifc.Name); err == nil {
		if as, err := ifi.Addrs(); err == nil {
			b.addrs = ipsOf(as)
		}
	}
	b.addrsAt = time.Now()
	return b.addrs
}

// sourceAddr: an address of the interface usable as source towards dst.
func (b *binder) sourceAddr(dst netip.Addr) (netip.Addr, error) {
	want4 := dst.Is4()
	var best netip.Addr
	for _, a := range b.currentAddrs() {
		if a.Is4() != want4 {
			continue
		}
		if !want4 && a.IsLinkLocalUnicast() && !dst.IsLinkLocalUnicast() {
			continue
		}
		if !best.IsValid() {
			best = a
		}
	}
	if !best.IsValid() {
		fam := "IPv4"
		if !want4 {
			fam = "IPv6"
		}
		return netip.Addr{}, fmt.Errorf("netbind: у интерфейса %s нет %s-адреса", b.ifc.Name, fam)
	}
	return best, nil
}

// DialContext: hostnames are resolved through LookupHost (the interface's
// DNS over the interface), then each address is tried in turn.
func (b *binder) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip.Unmap()}
	} else {
		ips, err = b.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	base, fam := splitNetwork(network)
	var cand []netip.Addr
	for _, ip := range ips {
		if (fam == '4' && !ip.Is4()) || (fam == '6' && ip.Is4()) {
			continue
		}
		cand = append(cand, ip)
	}
	if len(cand) == 0 {
		return nil, &net.AddrError{Err: "no suitable address", Addr: host}
	}
	var firstErr error
	for i, ip := range cand {
		dctx := ctx
		// Several addresses: like net.Dialer, give each a share of the
		// remaining time so one dead address does not eat it all.
		if dl, ok := ctx.Deadline(); ok && len(cand)-i > 1 {
			share := time.Until(dl) / time.Duration(len(cand)-i)
			if share < 2*time.Second {
				share = 2 * time.Second
			}
			var cancel context.CancelFunc
			dctx, cancel = context.WithTimeout(ctx, share)
			c, err := b.dialIP(dctx, base, ip, port)
			cancel()
			if err == nil {
				return c, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c, err := b.dialIP(dctx, base, ip, port)
		if err == nil {
			return c, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

func (b *binder) dialIP(ctx context.Context, base string, ip netip.Addr, port string) (net.Conn, error) {
	network := base + "4"
	if !ip.Is4() {
		network = base + "6"
	}
	laddr, err := b.localAddr(network, ip)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{LocalAddr: laddr, Control: b.control}
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

func (b *binder) ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error) {
	base, fam := splitNetwork(network)
	if base != "udp" {
		return nil, fmt.Errorf("netbind: ListenPacket %q не поддерживается", network)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", "0"
	}
	if host == "" {
		// Never a dual-stack socket: on Windows the unicast-if option is
		// per family, a dual-stack socket would send IPv4 the default way.
		if fam == 0 {
			fam = '4'
			if !hasFamily(b.currentAddrs(), true) && hasFamily(b.currentAddrs(), false) {
				fam = '6'
			}
		}
		probe := netip.IPv4Unspecified()
		if fam == '6' {
			probe = netip.IPv6Unspecified()
		}
		if la, err := b.localAddr("udp"+string(fam), probe); err != nil {
			return nil, err
		} else if la != nil {
			host = la.(*net.UDPAddr).IP.String()
		}
	} else if fam == 0 {
		fam = '4'
		if ip, err := netip.ParseAddr(host); err == nil && !ip.Unmap().Is4() {
			fam = '6'
		}
	}
	lc := net.ListenConfig{Control: b.control}
	return lc.ListenPacket(ctx, "udp"+string(fam), net.JoinHostPort(host, port))
}

// LookupHost: A + AAAA through the interface's DNS servers, one server at
// a time (an authoritative "no such host" ends the search: asking another
// server would only hide a DNS block). net.Resolver with PreferGo and our
// own Dial: the query itself goes over the interface, UDP first, TCP on
// truncation -- exactly how the Go resolver behaves, just bound.
func (b *binder) LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	servers, source := b.dnsServers, b.dnsSource
	// Rooted name: no search domains of the host appended.
	fqdn := host
	if !strings.HasSuffix(fqdn, ".") {
		fqdn += "."
	}
	var lastErr error
	for _, srv := range servers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		srv := srv
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			base, _ := splitNetwork(network)
			return b.dialIP(ctx, base, srv, "53")
		}}
		qctx, cancel := context.WithTimeout(ctx, perServerDNSTimeout)
		ips, err := r.LookupNetIP(qctx, "ip", fqdn)
		cancel()
		if err == nil && len(ips) > 0 {
			b.setLastSource(source)
			return orderAddrs(ips), nil
		}
		if err == nil {
			err = &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		lastErr = err
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			b.setLastSource(source)
			return nil, err
		}
	}
	if source == "public" {
		// Contract fallback: the interface has no DNS of its own and none
		// of the public resolvers answered over it -> the system resolver.
		// DNSInfo then reports "system" so the result can be marked.
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err == nil {
			b.setLastSource("system")
			return orderAddrs(ips), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = &net.DNSError{Err: "нет DNS-серверов", Name: host}
	}
	return nil, lastErr
}

func (b *binder) setLastSource(s string) {
	b.mu.Lock()
	b.lastSource = s
	b.mu.Unlock()
}

func (b *binder) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: newTransport(b.DialContext), Timeout: timeout}
}

// newTransport: a private pool per client; never a proxy from the
// environment (HTTPS_PROXY on a partner's machine must not reroute checks).
func newTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// ---- helpers ----

func splitNetwork(n string) (base string, fam byte) {
	switch {
	case strings.HasSuffix(n, "4"):
		return n[:len(n)-1], '4'
	case strings.HasSuffix(n, "6"):
		return n[:len(n)-1], '6'
	}
	return n, 0
}

func ipsOf(as []net.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range as {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if na, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, na.Unmap())
		}
	}
	return out
}

func hasFamily(as []netip.Addr, v4 bool) bool {
	for _, a := range as {
		if a.Is4() == v4 && !a.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func hasGlobal6(as []netip.Addr) bool {
	for _, a := range as {
		if a.Is6() && a.IsGlobalUnicast() && !a.IsPrivate() {
			return true
		}
	}
	return false
}

// orderAddrs: IPv4 first (mobile IPv6 is the less reliable family), the
// resolver's order kept within a family, duplicates dropped.
func orderAddrs(ips []netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		ip = ip.Unmap()
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Is4() && !out[j].Is4() })
	return out
}

// usableDNS: drops what is not a real per-interface server (loopback stubs
// such as systemd-resolved's 127.0.0.53, unspecified) and duplicates.
func usableDNS(in []netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, a := range in {
		a = a.Unmap()
		if !a.IsValid() || a.IsLoopback() || a.IsUnspecified() || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// Virtual / VPN interface names (both OSes use them in descriptions too).
var virtualNamePrefixes = []string{
	"tun", "tap", "wg", "tailscale", "zt", "docker", "veth", "br-", "virbr", "lxc", "lxd",
	"cni", "flannel", "cali", "vxlan", "kube", "cilium", "nordlynx", "proton", "ipsec",
	"vti", "gre", "gretap", "erspan", "ip6tnl", "ip_vti", "ip6_vti", "sit", "ip6gre", "dummy",
	"ifb", "nlmon", "vmnet", "vboxnet", "utun", "outline", "amn", "singbox", "sing-box",
	"xray", "clash", "mihomo", "hiddify", "warp", "cloudflarewarp", "nekoray",
}

func hasPrefixAny(s string, ps []string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// HiLink sticks and most LTE routers of that family hand out 192.168.8.0/24.
var hilinkGateway = netip.MustParseAddr("192.168.8.1")
