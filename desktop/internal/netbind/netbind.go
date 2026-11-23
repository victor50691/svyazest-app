// Package netbind: everything that has to leave the machine through ONE
// chosen network interface (the partner's modem) or, for the control
// channel, through anything BUT it. Owner: agent "net+packaging".
//
// CONTRACT (other packages compile against these signatures; keep them):
package netbind

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Iface describes one network interface of the host.
type Iface struct {
	Name    string       // "wwan0", "usb0", "eth1", Windows: adapter friendly name
	Index   int          // OS interface index
	Addrs   []netip.Addr // its unicast addresses
	DNS     []netip.Addr // DNS servers that belong to THIS interface (DHCP/bearer), may be empty
	Gateway netip.Addr   // default gateway via this interface, zero if none
	Kind    string       // "modem" | "ethernet" | "wifi" | "loopback" | "virtual" | "other"
	Up      bool
}

// Binder sends traffic out through one interface only.
type Binder interface {
	Iface() Iface
	// DialContext dials bound to the interface (Linux SO_BINDTODEVICE,
	// Windows IP_UNICAST_IF/IPV6_UNICAST_IF + bind to its address).
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// ListenPacket opens a UDP socket bound to the interface.
	ListenPacket(ctx context.Context, network, addr string) (net.PacketConn, error)
	// LookupHost resolves through the interface's own DNS servers over the
	// interface (fallback: the system resolver only if the iface has no DNS;
	// ok=false in that case is reported by DNSInfo).
	LookupHost(ctx context.Context, host string) ([]netip.Addr, error)
	// DNSInfo: which servers LookupHost uses and whether they are the
	// interface's own ("iface") or a fallback ("system"/"public").
	DNSInfo() (servers []netip.Addr, source string)
	// HTTPClient: dialer + resolver above, no proxy from env, no keep-alive
	// sharing with other clients. timeout 0 = none.
	HTTPClient(timeout time.Duration) *http.Client
	// Ping: one ICMP echo out of the interface. err != nil = no reply.
	Ping(ctx context.Context, ip netip.Addr, timeout time.Duration) (time.Duration, error)
	// XraySockopt: the "sockopt" object to put into every xray outbound so
	// xray's own sockets go out of this interface (e.g. {"interface":"wwan0"}).
	XraySockopt() map[string]any
	// Counters: total rx/tx bytes of the interface (for traffic_bytes delta).
	Counters() (rx, tx uint64, err error)
}

// List returns the host's interfaces (Kind filled in as well as possible).
func List() ([]Iface, error) { return listImpl() }

// ForInterface returns a Binder for the interface with this name.
func ForInterface(name string) (Binder, error) { return forInterfaceImpl(name) }

// Control is the HTTP client for the DPI//Checker API. It must NOT use the
// modem when anything else works: route order = every other "up" interface
// with a gateway (ethernet, wifi, ...), then the modem. Remembers the last
// working route, a failed route goes to the end for 60 s (like the Android
// ControlChannel.kt). Fallback to the next route only on connect errors,
// never re-sends a request whose body may have been sent.
type Control interface {
	Do(req *http.Request) (*http.Response, error)
	// LastRoute: value for the X-Net-Route header / UI, e.g. "ethernet:eth0",
	// "wifi:wlan0", "cellular" (= the modem), "default".
	LastRoute() string
	// SetModem changes which interface is "the modem" (excluded / last).
	SetModem(name string)
}

func NewControl(modemIface string) Control { return newControlImpl(modemIface) }
