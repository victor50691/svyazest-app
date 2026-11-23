//go:build linux

package netbind

// Linux: interfaces from net.Interfaces + /sys/class/net, default routes
// from /proc/net/route and /proc/net/ipv6_route, per-interface DNS from
// whatever manages the link (systemd-resolved/networkd, NetworkManager,
// dhclient/dhcpcd leases, ModemManager bearers). Sockets are pinned with
// SO_BINDTODEVICE.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type bindMode int

const (
	modeDevice bindMode = iota // SO_BINDTODEVICE works
	modeAddr                   // no permission: bind source address + route check
)

type osBinder struct {
	mode bindMode
}

func forInterfaceImpl(name string) (Binder, error) {
	list, err := listDetailedImpl(true)
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		if d.Name != name {
			continue
		}
		b := newBinder(d)
		if err := probeBindToDevice(name); err != nil {
			if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
				return nil, fmt.Errorf("netbind: привязка к %s: %w", name, err)
			}
			// Kernels before 5.7 want CAP_NET_RAW for SO_BINDTODEVICE.
			// Without it we can still bind the source address, and we only
			// let a socket through when the kernel routes that destination
			// from that source out of this very interface (policy routing),
			// so a check never silently goes out of eth0 instead.
			b.os.mode = modeAddr
			b.warn = "нет права CAP_NET_RAW: привязка к интерфейсу " + name +
				" невозможна, используется адрес интерфейса; адреса, маршрут к которым идёт не через него, будут недоступны. Запустите агент от root или дайте CAP_NET_RAW."
		}
		return b, nil
	}
	return nil, fmt.Errorf("netbind: интерфейс %q не найден", name)
}

func probeBindToDevice(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
	}
	defer unix.Close(fd)
	return unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
}

// control runs on every socket before connect/bind.
func (b *binder) control(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) { serr = b.pinFD(int(fd), address) })
	if err != nil {
		return err
	}
	return serr
}

// pinFD: SO_BINDTODEVICE, or in address mode the route check. address is
// the remote "ip:port" for dials, the local one for listens.
func (b *binder) pinFD(fd int, address string) error {
	if b.os.mode == modeDevice {
		return unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, b.ifc.Name)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	dst, err := netip.ParseAddr(host)
	if err != nil || dst.IsUnspecified() || b.isOwnAddr(dst) {
		return nil // listen socket: the bound local address is all we can do
	}
	src, err := b.sourceAddr(dst.Unmap())
	if err != nil {
		return err
	}
	oif, err := routeOif(dst.Unmap(), src)
	if err != nil {
		return fmt.Errorf("netbind: маршрут к цели не определён: %w", err)
	}
	if oif != b.ifc.Index {
		return fmt.Errorf("netbind: нет прав на привязку к %s (нужен CAP_NET_RAW), а маршрут к цели идёт через другой интерфейс", b.ifc.Name)
	}
	return nil
}

func (b *binder) isOwnAddr(a netip.Addr) bool {
	for _, x := range b.currentAddrs() {
		if x == a.Unmap() {
			return true
		}
	}
	return false
}

// localAddr: with SO_BINDTODEVICE the kernel picks the source itself (no
// bind needed); in address mode the socket is bound to the interface IP.
func (b *binder) localAddr(network string, dst netip.Addr) (net.Addr, error) {
	if b.os.mode == modeDevice {
		return nil, nil
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
	read := func(f string) (uint64, error) {
		s, err := os.ReadFile(filepath.Join("/sys/class/net", b.ifc.Name, "statistics", f))
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(s)), 10, 64)
	}
	if rx, err = read("rx_bytes"); err != nil {
		return 0, 0, err
	}
	tx, err = read("tx_bytes")
	return rx, tx, err
}

// routeOif asks the kernel (RTM_GETROUTE, like `ip route get DST from SRC`)
// which interface it would send DST from SRC out of.
func routeOif(dst, src netip.Addr) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, err
	}
	family, bits := byte(unix.AF_INET), byte(32)
	if !dst.Is4() {
		family, bits = unix.AF_INET6, 128
	}
	attr := func(typ uint16, data []byte) []byte {
		l := unix.SizeofRtAttr + len(data)
		b := make([]byte, (l+3)&^3)
		binary.NativeEndian.PutUint16(b[0:], uint16(l))
		binary.NativeEndian.PutUint16(b[2:], typ)
		copy(b[4:], data)
		return b
	}
	rtm := make([]byte, unix.SizeofRtMsg)
	rtm[0] = family // rtm_family
	rtm[1] = bits   // rtm_dst_len
	rtm[2] = bits   // rtm_src_len
	body := append(rtm, attr(unix.RTA_DST, dst.AsSlice())...)
	body = append(body, attr(unix.RTA_SRC, src.AsSlice())...)
	msg := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(body))
	binary.NativeEndian.PutUint32(msg[0:], uint32(unix.SizeofNlMsghdr+len(body)))
	binary.NativeEndian.PutUint16(msg[4:], unix.RTM_GETROUTE)
	binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(msg[8:], 1)
	msg = append(msg, body...)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, err
	}
	tv := unix.Timeval{Sec: 2}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	buf := make([]byte, 8192)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		switch m.Header.Type {
		case unix.NLMSG_ERROR:
			if len(m.Data) >= 4 {
				if e := int32(binary.NativeEndian.Uint32(m.Data)); e != 0 {
					return 0, syscall.Errno(-e)
				}
			}
		case unix.RTM_NEWROUTE:
			attrs, err := syscall.ParseNetlinkRouteAttr(&m)
			if err != nil {
				return 0, err
			}
			for _, a := range attrs {
				if a.Attr.Type == unix.RTA_OIF && len(a.Value) >= 4 {
					return int(binary.NativeEndian.Uint32(a.Value)), nil
				}
			}
		}
	}
	return 0, errors.New("в ответе ядра нет интерфейса")
}

// ---------------------------------------------------------------- List

type defRoute struct {
	gw4, gw6         netip.Addr
	has4, has6       bool
	metric4, metric6 int
}

func listDetailedImpl(withDNS bool) ([]IfaceDetail, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	routes := readDefaultRoutes()
	out := make([]IfaceDetail, 0, len(ifs))
	for _, ifi := range ifs {
		d := IfaceDetail{Iface: Iface{Name: ifi.Name, Index: ifi.Index, Up: ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagRunning != 0}}
		if as, err := ifi.Addrs(); err == nil {
			for _, a := range ipsOf(as) {
				// fe80:: is no use for reaching anything outside the link.
				if a.Is6() && a.IsLinkLocalUnicast() {
					continue
				}
				d.Addrs = append(d.Addrs, a)
			}
		}
		if r, ok := routes[ifi.Name]; ok {
			d.DefaultRoute = r.has4 || r.has6
			if r.gw4.IsValid() {
				d.Gateway = r.gw4
			} else if r.gw6.IsValid() {
				d.Gateway = r.gw6
			}
		}
		d.Kind, d.Driver, d.Description = classifyLinux(ifi, d.Gateway, d.DefaultRoute)
		out = append(out, d)
	}
	if withDNS {
		dns := linuxDNS(out)
		for i := range out {
			out[i].DNS = dns[out[i].Name]
		}
	}
	return out, nil
}

// readDefaultRoutes: the main table's default routes per interface, the
// lowest metric wins. A default route without a gateway (point-to-point
// "default dev wwan0") still counts as DefaultRoute.
func readDefaultRoutes() map[string]*defRoute {
	res := map[string]*defRoute{}
	get := func(n string) *defRoute {
		r := res[n]
		if r == nil {
			r = &defRoute{metric4: 1 << 30, metric6: 1 << 30}
			res[n] = r
		}
		return r
	}
	if f, err := os.Open("/proc/net/route"); err == nil {
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) < 8 || fl[1] != "00000000" || fl[7] != "00000000" {
				continue
			}
			flags, _ := strconv.ParseUint(fl[3], 16, 32)
			if flags&0x1 == 0 { // RTF_UP
				continue
			}
			metric, _ := strconv.Atoi(fl[6])
			r := get(fl[0])
			if metric >= r.metric4 && r.has4 {
				continue
			}
			r.has4, r.metric4 = true, metric
			r.gw4 = netip.Addr{}
			if gw, err := strconv.ParseUint(fl[2], 16, 32); err == nil && gw != 0 {
				var b [4]byte
				binary.LittleEndian.PutUint32(b[:], uint32(gw))
				r.gw4 = netip.AddrFrom4(b)
			}
		}
		f.Close()
	}
	if f, err := os.Open("/proc/net/ipv6_route"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) < 10 || fl[0] != strings.Repeat("0", 32) || fl[1] != "00" || fl[9] == "lo" {
				continue
			}
			flags, _ := strconv.ParseUint(fl[8], 16, 32)
			if flags&0x1 == 0 || flags&0x200 != 0 { // !RTF_UP || RTF_REJECT
				continue
			}
			metric64, _ := strconv.ParseUint(fl[5], 16, 32)
			metric := int(metric64)
			r := get(fl[9])
			if metric >= r.metric6 && r.has6 {
				continue
			}
			r.has6, r.metric6 = true, metric
			r.gw6 = netip.Addr{}
			if raw, err := hex.DecodeString(fl[4]); err == nil && len(raw) == 16 {
				if a := netip.AddrFrom16([16]byte(raw)); !a.IsUnspecified() {
					r.gw6 = a
				}
			}
		}
		f.Close()
	}
	return res
}

// Drivers of USB / PCIe cellular modems and of phones sharing their
// connection over USB. cdc_ether/cdc_ncm are generic: HiLink sticks and
// phones use them, but so do some USB Ethernet adapters -- see usbEthVendors.
var modemDrivers = map[string]bool{
	"qmi_wwan": true, "qmi_wwan_q": true, "cdc_mbim": true, "huawei_cdc_ncm": true,
	"rndis_host": true, "ipheth": true, "sierra_net": true, "mhi_net": true, "mhi_wwan_mbim": true,
	"option": true, "hso": true, "GobiNet": true, "cdc_ether": true, "cdc_ncm": true, "cdc_eem": true,
	"t7xx": true, "iosm": true, "mtk_t7xx": true, "rmnet": true, "rmnet_ipa": true,
}

// USB vendor ids of Ethernet-adapter chips / docks that use cdc_ether or
// cdc_ncm: those are wired Ethernet, not a modem.
var usbEthVendors = map[string]bool{
	"0bda": true, // Realtek
	"0b95": true, // ASIX
	"0424": true, // Microchip/SMSC
	"17ef": true, // Lenovo docks
	"413c": true, // Dell docks
	"2357": true, // TP-Link
	"050d": true, // Belkin
	"2001": true, // D-Link
	"13b1": true, // Linksys
	"03f0": true, // HP docks
	"0fe6": true, // ICS/Kontron
	"2109": true, // VIA Labs hubs
	"1c04": true, // QinHeng
}

// classifyLinux: Kind, driver and a human description of the device.
func classifyLinux(ifi net.Interface, gw netip.Addr, defRoute bool) (kind, driver, desc string) {
	if ifi.Flags&net.FlagLoopback != 0 {
		return "loopback", "", ""
	}
	sys := filepath.Join("/sys/class/net", ifi.Name)
	devtype := ueventValue(filepath.Join(sys, "uevent"), "DEVTYPE")
	if lnk, err := os.Readlink(filepath.Join(sys, "device", "driver")); err == nil {
		driver = filepath.Base(lnk)
	}
	dev, _ := filepath.EvalSymlinks(filepath.Join(sys, "device"))
	isUSB := strings.Contains(dev, "/usb")
	vendor := ""
	if isUSB {
		// device/ is the USB interface (1-1.3:1.0); the device with ids
		// and strings is its parent.
		vendor = readTrim(filepath.Join(dev, "..", "idVendor"))
		desc = strings.TrimSpace(readTrim(filepath.Join(dev, "..", "manufacturer")) + " " + readTrim(filepath.Join(dev, "..", "product")))
	}
	name := ifi.Name
	_, tunErr := os.Stat(filepath.Join(sys, "tun_flags"))
	switch {
	case devtype == "wwan" || strings.HasPrefix(name, "wwan") || strings.HasPrefix(name, "rmnet") || strings.HasPrefix(name, "ppp"):
		// ppp*: a 3G/4G stick driven by pppd/ModemManager. PPPoE uplinks
		// also show up as ppp*, then the control channel just tries it last.
		return "modem", driver, desc
	case tunErr == nil || devtype == "wireguard" || hasPrefixAny(name, virtualNamePrefixes):
		return "virtual", driver, desc
	case devtype == "wlan" || exists(filepath.Join(sys, "wireless")) || exists(filepath.Join(sys, "phy80211")):
		return "wifi", driver, desc
	case devtype == "bridge" || devtype == "bond" || devtype == "vlan" || devtype == "macvlan":
		// A bridge/bond carrying the host's uplink is its Ethernet; one
		// without a default route (docker0, lxcbr0) is internal.
		if defRoute {
			return "ethernet", driver, desc
		}
		return "virtual", driver, desc
	case modemDrivers[driver]:
		if (driver == "cdc_ether" || driver == "cdc_ncm" || driver == "cdc_eem") && usbEthVendors[vendor] && gw != hilinkGateway {
			return "ethernet", driver, desc
		}
		return "modem", driver, desc
	case dev == "":
		return "virtual", driver, desc
	case isUSB && gw == hilinkGateway:
		return "modem", driver, desc
	}
	if t := readTrim(filepath.Join(sys, "type")); t == "1" {
		return "ethernet", driver, desc
	}
	return "other", driver, desc
}

func ueventValue(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---------------------------------------------------------------- DNS

var (
	dnsCacheMu sync.Mutex
	dnsCache   map[string][]netip.Addr
	dnsCacheAt time.Time
)

// linuxDNS: per-interface DNS servers from every source we know, first
// source that knows an interface wins. Cached 5 s: List is called from the
// UI and the control channel, the sources are external commands.
func linuxDNS(ifs []IfaceDetail) map[string][]netip.Addr {
	dnsCacheMu.Lock()
	defer dnsCacheMu.Unlock()
	if dnsCache != nil && time.Since(dnsCacheAt) < 5*time.Second {
		return dnsCache
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := map[string][]netip.Addr{}
	byIndex := map[int]string{}
	for _, d := range ifs {
		byIndex[d.Index] = d.Name
	}
	add := func(name string, addrs []netip.Addr) {
		addrs = usableDNS(addrs)
		if name == "" || len(addrs) == 0 || len(res[name]) > 0 {
			return
		}
		res[name] = addrs
	}
	missing := func() []IfaceDetail {
		var m []IfaceDetail
		for _, d := range ifs {
			if d.Kind != "loopback" && d.Up && len(res[d.Name]) == 0 {
				m = append(m, d)
			}
		}
		return m
	}

	// 1. systemd-resolved: per-link servers ("Link 3 (eth0): 192.168.1.1").
	if out, err := runCmd(ctx, 2*time.Second, "resolvectl", "dns"); err == nil {
		for name, addrs := range parseResolvectl(out) {
			add(name, addrs)
		}
	}
	// 1b. the files resolved and networkd keep per ifindex (also present
	// when resolvectl is not installed, e.g. minimal images).
	for idx, name := range byIndex {
		for _, f := range []string{
			fmt.Sprintf("/run/systemd/resolve/netif/%d", idx),
			fmt.Sprintf("/run/systemd/netif/links/%d", idx),
			fmt.Sprintf("/run/systemd/netif/leases/%d", idx),
		} {
			add(name, parseKeyAddrs(f, "SERVERS", "DNS"))
		}
	}
	// 2. NetworkManager.
	if len(missing()) > 0 {
		if out, err := runCmd(ctx, 2*time.Second, "nmcli", "-t", "-f", "GENERAL.DEVICE,IP4.DNS,IP6.DNS", "dev", "show"); err == nil {
			for name, addrs := range parseNmcli(out) {
				add(name, addrs)
			}
		}
	}
	// 3. dhclient lease files (Debian ifupdown, older NetworkManager).
	if len(missing()) > 0 {
		var files []string
		for _, g := range []string{"/var/lib/dhcp/dhclient*.lease*", "/var/lib/dhclient/*.lease*", "/var/lib/NetworkManager/dhclient-*.lease"} {
			m, _ := filepath.Glob(g)
			files = append(files, m...)
		}
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				for name, addrs := range parseDhclientLeases(b) {
					add(name, addrs)
				}
			}
		}
	}
	// 4. dhcpcd (Raspberry Pi OS before Bookworm): `dhcpcd -U IF` dumps the lease.
	if _, err := exec.LookPath("dhcpcd"); err == nil {
		for _, d := range missing() {
			if d.Kind == "virtual" {
				continue
			}
			if out, err := runCmd(ctx, time.Second, "dhcpcd", "-U", d.Name); err == nil {
				add(d.Name, parseDhcpcdDump(out))
			}
		}
	}
	// 5. ModemManager bearers: a modem set up by MM alone (no NM) has its
	// DNS only there.
	needMM := false
	for _, d := range missing() {
		if d.Kind == "modem" {
			needMM = true
		}
	}
	if needMM {
		for name, addrs := range mmBearerDNS(ctx) {
			add(name, addrs)
		}
	}
	// 6. A HiLink stick / tethering phone is a small NAT router whose DHCP
	// always names the router itself as DNS (dnsmasq forwarding to the
	// operator). If no source above knew the lease, the gateway is that
	// server -- still the interface's own DNS path, so source stays "iface".
	for _, d := range missing() {
		if d.Kind == "modem" && d.Gateway.Is4() && d.Gateway.IsPrivate() {
			add(d.Name, []netip.Addr{d.Gateway})
		}
	}
	dnsCache, dnsCacheAt = res, time.Now()
	return res
}

func runCmd(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Output()
}

var reResolvectlLink = regexp.MustCompile(`^Link\s+\d+\s+\(([^)]+)\):(.*)$`)

func parseResolvectl(out []byte) map[string][]netip.Addr {
	res := map[string][]netip.Addr{}
	for _, l := range strings.Split(string(out), "\n") {
		m := reResolvectlLink.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		res[m[1]] = parseAddrList(m[2])
	}
	return res
}

// parseAddrList: "1.1.1.1#cloudflare-dns.com fe80::1%eth0 8.8.8.8" and
// comma-separated variants.
func parseAddrList(s string) []netip.Addr {
	var out []netip.Addr
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == ';' }) {
		f = strings.Trim(f, `"'`)
		if i := strings.IndexByte(f, '#'); i >= 0 {
			f = f[:i]
		}
		if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, a.Unmap())
		}
	}
	return out
}

func parseKeyAddrs(path string, keys ...string) []netip.Addr {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, l := range strings.Split(string(b), "\n") {
		for _, k := range keys {
			if v, ok := strings.CutPrefix(l, k+"="); ok {
				out = append(out, parseAddrList(v)...)
			}
		}
	}
	return out
}

// nmcli -t escapes ':' inside values as "\:".
func parseNmcli(out []byte) map[string][]netip.Addr {
	res := map[string][]netip.Addr{}
	dev := ""
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.ReplaceAll(v, `\:`, ":")
		switch {
		case k == "GENERAL.DEVICE":
			dev = v
		case strings.HasPrefix(k, "IP4.DNS") || strings.HasPrefix(k, "IP6.DNS"):
			res[dev] = append(res[dev], parseAddrList(v)...)
		}
	}
	return res
}

var (
	reDhclientIface = regexp.MustCompile(`interface\s+"([^"]+)"`)
	reDhclientDNS   = regexp.MustCompile(`option\s+domain-name-servers\s+([^;]+);`)
)

// parseDhclientLeases: the LAST lease block per interface wins (dhclient
// appends renewals).
func parseDhclientLeases(b []byte) map[string][]netip.Addr {
	res := map[string][]netip.Addr{}
	for _, blk := range strings.Split(string(b), "lease {")[1:] {
		mi := reDhclientIface.FindStringSubmatch(blk)
		md := reDhclientDNS.FindStringSubmatch(blk)
		if mi != nil && md != nil {
			res[mi[1]] = parseAddrList(md[1])
		}
	}
	return res
}

func parseDhcpcdDump(out []byte) []netip.Addr {
	var res []netip.Addr
	for _, l := range strings.Split(string(out), "\n") {
		for _, k := range []string{"domain_name_servers=", "dhcp6_name_servers="} {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), k); ok {
				res = append(res, parseAddrList(v)...)
			}
		}
	}
	return res
}

// mmBearerDNS: interface -> DNS of every connected ModemManager bearer.
func mmBearerDNS(ctx context.Context) map[string][]netip.Addr {
	res := map[string][]netip.Addr{}
	out, err := runCmd(ctx, 2*time.Second, "mmcli", "-L", "-J")
	if err != nil {
		return res
	}
	var list struct {
		Modems []string `json:"modem-list"`
	}
	if json.Unmarshal(out, &list) != nil {
		return res
	}
	for _, mp := range list.Modems {
		out, err := runCmd(ctx, 2*time.Second, "mmcli", "-m", lastPathElem(mp), "-J")
		if err != nil {
			continue
		}
		var m struct {
			Modem struct {
				Generic struct {
					Bearers []string `json:"bearers"`
				} `json:"generic"`
			} `json:"modem"`
		}
		if json.Unmarshal(out, &m) != nil {
			continue
		}
		for _, bp := range m.Modem.Generic.Bearers {
			out, err := runCmd(ctx, 2*time.Second, "mmcli", "-b", lastPathElem(bp), "-J")
			if err != nil {
				continue
			}
			var br struct {
				Bearer struct {
					Status struct {
						Connected string `json:"connected"`
						Interface string `json:"interface"`
					} `json:"status"`
					IPv4 struct {
						DNS []string `json:"dns"`
					} `json:"ipv4-config"`
					IPv6 struct {
						DNS []string `json:"dns"`
					} `json:"ipv6-config"`
				} `json:"bearer"`
			}
			if json.Unmarshal(out, &br) != nil || br.Bearer.Status.Connected != "yes" {
				continue
			}
			ifn := br.Bearer.Status.Interface
			res[ifn] = append(res[ifn], parseAddrList(strings.Join(append(br.Bearer.IPv4.DNS, br.Bearer.IPv6.DNS...), " "))...)
		}
	}
	return res
}

func lastPathElem(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
