//go:build windows

package netbind

// Windows: adapters from GetAdaptersAddresses (addresses, DNS, gateways in
// one call) plus GetIpForwardTable2 for default routes without a gateway
// (point-to-point WWAN). Sockets are pinned with IP_UNICAST_IF /
// IPV6_UNICAST_IF -- the per-socket "send out of this interface" option --
// and bound to the interface's address so the source matches.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ipUnicastIf   = 31 // IP_UNICAST_IF (ws2ipdef.h)
	ipv6UnicastIf = 31 // IPV6_UNICAST_IF
)

type osBinder struct {
	idx4, idx6 uint32
	luid       uint64
}

func forInterfaceImpl(name string) (Binder, error) {
	list, err := listWindows()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.d.Name != name {
			continue
		}
		b := newBinder(a.d)
		b.os = osBinder{idx4: a.idx4, idx6: a.idx6, luid: a.luid}
		return b, nil
	}
	return nil, fmt.Errorf("netbind: интерфейс %q не найден", name)
}

func (b *binder) control(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) { serr = b.pinHandle(windows.Handle(fd), network) })
	if err != nil {
		return err
	}
	return serr
}

// pinHandle: Go passes "tcp4"/"udp6"... by the socket's family.
func (b *binder) pinHandle(h windows.Handle, network string) error {
	_, fam := splitNetwork(network)
	if fam == '4' {
		if b.os.idx4 == 0 {
			return fmt.Errorf("netbind: у интерфейса %s нет IPv4", b.ifc.Name)
		}
		// IP_UNICAST_IF takes the index in NETWORK byte order (the docs:
		// "the interface index in network byte order"); IPV6_UNICAST_IF in
		// host order. Same trick as xray's sockopt_windows.go.
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], b.os.idx4)
		v := int(binary.NativeEndian.Uint32(be[:]))
		if err := windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, v); err != nil {
			return fmt.Errorf("netbind: IP_UNICAST_IF: %w", err)
		}
		return nil
	}
	if b.os.idx6 == 0 {
		return fmt.Errorf("netbind: у интерфейса %s нет IPv6", b.ifc.Name)
	}
	if err := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, int(b.os.idx6)); err != nil {
		return fmt.Errorf("netbind: IPV6_UNICAST_IF: %w", err)
	}
	return nil
}

// localAddr: always bind to the interface's address on Windows; with the
// weak host model the unicast-if option alone may still pick another
// adapter's source address.
func (b *binder) localAddr(network string, dst netip.Addr) (net.Addr, error) {
	if dst.IsUnspecified() {
		// ListenPacket without a host: pick the family's address.
		src, err := b.sourceAddr(dst)
		if err != nil {
			return nil, err
		}
		return &net.UDPAddr{IP: src.AsSlice()}, nil
	}
	src, err := b.sourceAddr(dst)
	if err != nil {
		return nil, err
	}
	base, _ := splitNetwork(network)
	if base == "udp" {
		return &net.UDPAddr{IP: src.AsSlice()}, nil
	}
	return &net.TCPAddr{IP: src.AsSlice()}, nil
}

func (b *binder) Counters() (rx, tx uint64, err error) {
	var row windows.MibIfRow2
	row.InterfaceLuid = b.os.luid
	row.InterfaceIndex = b.os.idx4
	if row.InterfaceIndex == 0 {
		row.InterfaceIndex = b.os.idx6
	}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormal, &row); err != nil {
		return 0, 0, err
	}
	return row.InOctets, row.OutOctets, nil
}

// ---------------------------------------------------------------- List

type winAdapter struct {
	d          IfaceDetail
	idx4, idx6 uint32
	luid       uint64
}

func listDetailedImpl(withDNS bool) ([]IfaceDetail, error) {
	l, err := listWindows()
	if err != nil {
		return nil, err
	}
	out := make([]IfaceDetail, len(l))
	for i := range l {
		out[i] = l[i].d
		if !withDNS {
			out[i].DNS = nil
		}
	}
	return out, nil
}

// Windows lists these "well-known site-local" IPv6 DNS placeholders on
// adapters that never got an IPv6 DNS server.
var siteLocalDNS = netip.MustParsePrefix("fec0:0:0:ffff::/64")

func listWindows() ([]winAdapter, error) {
	var buf []byte
	size := uint32(16 * 1024)
	flags := uint32(windows.GAA_FLAG_INCLUDE_GATEWAYS | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST)
	var first *windows.IpAdapterAddresses
	for i := 0; i < 4; i++ {
		buf = make([]byte, size)
		first = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) || i == 3 {
			return nil, fmt.Errorf("netbind: GetAdaptersAddresses: %w", err)
		}
	}
	defRoutes := defaultRoutesByIndex()
	var out []winAdapter
	for a := first; a != nil; a = a.Next {
		w := winAdapter{idx4: a.IfIndex, idx6: a.Ipv6IfIndex, luid: a.Luid}
		d := &w.d
		d.Name = windows.UTF16PtrToString(a.FriendlyName)
		d.Description = windows.UTF16PtrToString(a.Description)
		d.Index = int(a.IfIndex)
		if d.Index == 0 {
			d.Index = int(a.Ipv6IfIndex)
		}
		d.Up = a.OperStatus == windows.IfOperStatusUp
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			if ip, ok := sockAddr(u.Address); ok && !(ip.Is6() && ip.IsLinkLocalUnicast()) {
				d.Addrs = append(d.Addrs, ip)
			}
		}
		for s := a.FirstDnsServerAddress; s != nil; s = s.Next {
			if ip, ok := sockAddr(s.Address); ok && !siteLocalDNS.Contains(ip) {
				d.DNS = append(d.DNS, ip)
			}
		}
		d.DNS = usableDNS(d.DNS)
		var gw6 netip.Addr
		for g := a.FirstGatewayAddress; g != nil; g = g.Next {
			ip, ok := sockAddr(g.Address)
			if !ok || ip.IsUnspecified() {
				continue
			}
			if ip.Is4() && !d.Gateway.IsValid() {
				d.Gateway = ip
			} else if ip.Is6() && !gw6.IsValid() {
				gw6 = ip
			}
		}
		if !d.Gateway.IsValid() {
			d.Gateway = gw6
		}
		r := defRoutes[a.IfIndex] || defRoutes[a.Ipv6IfIndex]
		d.DefaultRoute = r || d.Gateway.IsValid()
		d.Kind = classifyWindows(a.IfType, d.Name, d.Description, d.Gateway)
		out = append(out, w)
	}
	return out, nil
}

// defaultRoutesByIndex: interfaces with a 0.0.0.0/0 or ::/0 route.
func defaultRoutesByIndex() map[uint32]bool {
	res := map[uint32]bool{}
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &t); err != nil || t == nil {
		return res
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength == 0 && r.Loopback == 0 {
			res[r.InterfaceIndex] = true
		}
	}
	return res
}

func sockAddr(sa windows.SocketAddress) (netip.Addr, bool) {
	ip := sa.IP()
	if ip == nil {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ip)
	return a.Unmap(), ok
}

var winVirtualHints = []string{
	"tap-windows", "tap-", "wireguard", "wintun", "hyper-v", "virtualbox", "vmware", "tailscale",
	"openvpn", "zerotier", "wan miniport", "npcap", "loopback", "vpn", "teredo", "isatap",
	"pseudo-interface", "virtual", "docker", "vethernet", "wsl", "amnezia", "outline", "nordlynx",
	"cloudflare warp", "sing-tun", "hiddify", "clash", "mihomo", "v2ray", "xray", "nekoray",
}

// Phones sharing their connection over USB and USB modems that present
// themselves as a network card.
var winModemHints = []string{
	"remote ndis", "rndis", "apple mobile device ethernet", "mobile broadband", "mobile connect",
	"huawei", "zte", "quectel", "sierra wireless", "fibocom", "telit", "netgear aircard",
	"usb ncm", "ncm device", "modem", "lte", "wwan", "android", "samsung mobile", "iphone",
}

func classifyWindows(ifType uint32, name, desc string, gw netip.Addr) string {
	d := strings.ToLower(desc + " " + name)
	switch {
	case ifType == 24: // IF_TYPE_SOFTWARE_LOOPBACK
		return "loopback"
	case ifType == 243 || ifType == 244: // IF_TYPE_WWANPP / WWANPP2
		return "modem"
	case containsAny(d, winVirtualHints) || ifType == 131 || ifType == 53 || ifType == 23:
		// 131 tunnel, 53 prop virtual, 23 PPP (VPN connections on Windows)
		return "virtual"
	case ifType == 71: // IEEE 802.11
		return "wifi"
	case containsAny(d, winModemHints) || gw == hilinkGateway:
		return "modem"
	case ifType == 6:
		return "ethernet"
	}
	return "other"
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
