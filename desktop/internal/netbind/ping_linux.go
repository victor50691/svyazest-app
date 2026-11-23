//go:build linux

package netbind

// ICMP echo out of one interface. First the unprivileged "ping socket"
// (SOCK_DGRAM + IPPROTO_ICMP, allowed when our gid is in
// net.ipv4.ping_group_range -- the default on Debian/Raspberry Pi OS), then
// a raw socket (root / CAP_NET_RAW). Either is pinned like every other
// socket (SO_BINDTODEVICE or source address + route check).

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

func (b *binder) Ping(ctx context.Context, ip netip.Addr, timeout time.Duration) (time.Duration, error) {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return 0, errors.New("netbind: пустой адрес")
	}
	fam, proto := unix.AF_INET, unix.IPPROTO_ICMP
	if !ip.Is4() {
		fam, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	raw := false
	fd, err := unix.Socket(fam, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		fd, err = unix.Socket(fam, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
		if err != nil {
			return 0, fmt.Errorf("netbind: ICMP недоступен (нет ping_group_range и CAP_NET_RAW): %w", err)
		}
		raw = true
	}
	if err := b.pinFD(fd, net.JoinHostPort(ip.String(), "0")); err != nil {
		unix.Close(fd)
		return 0, err
	}
	if b.os.mode == modeAddr {
		src, err := b.sourceAddr(ip)
		if err != nil {
			unix.Close(fd)
			return 0, err
		}
		var sa unix.Sockaddr
		if src.Is4() {
			sa = &unix.SockaddrInet4{Addr: src.As4()}
		} else {
			sa = &unix.SockaddrInet6{Addr: src.As16()}
		}
		if err := unix.Bind(fd, sa); err != nil {
			unix.Close(fd)
			return 0, err
		}
	}
	f := os.NewFile(uintptr(fd), "icmp")
	pc, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		return 0, err
	}
	defer pc.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if timeout <= 0 {
		if _, ok := ctx.Deadline(); !ok {
			deadline = time.Now().Add(5 * time.Second)
		}
	}
	_ = pc.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = pc.SetDeadline(time.Now()) })
	defer stop()

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	id := int(binary.BigEndian.Uint16(rnd[:2]))
	seq := int(binary.BigEndian.Uint16(rnd[2:]))
	var typ, reply icmp.Type = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	protoNum := 1
	if !ip.Is4() {
		typ, reply, protoNum = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, 58
	}
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("svyazest-agent-ping-0123456789ab")}}
	wb, err := msg.Marshal(nil) // ICMPv6 checksum is filled in by the kernel
	if err != nil {
		return 0, err
	}
	var dst net.Addr = &net.UDPAddr{IP: ip.AsSlice()}
	if raw {
		dst = &net.IPAddr{IP: ip.AsSlice()}
	}
	start := time.Now()
	if _, err := pc.WriteTo(wb, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("netbind: нет ответа на ping: %w", err)
		}
		data := buf[:n]
		// Raw IPv4 sockets hand us the IP header too.
		if raw && ip.Is4() && len(data) > 20 && data[0]>>4 == 4 {
			hl := int(data[0]&0x0f) * 4
			if hl < len(data) {
				data = data[hl:]
			}
		}
		if !sameIP(from, ip) {
			continue
		}
		m, err := icmp.ParseMessage(protoNum, data)
		if err != nil || m.Type != reply {
			continue
		}
		e, ok := m.Body.(*icmp.Echo)
		// Ping sockets rewrite the ID to the socket's "port"; only raw
		// sockets see every echo reply on the host, so only they check it.
		if !ok || e.Seq != seq || (raw && e.ID != id) {
			continue
		}
		return time.Since(start), nil
	}
}

func sameIP(a net.Addr, ip netip.Addr) bool {
	var x net.IP
	switch v := a.(type) {
	case *net.UDPAddr:
		x = v.IP
	case *net.IPAddr:
		x = v.IP
	default:
		return false
	}
	na, ok := netip.AddrFromSlice(x)
	return ok && na.Unmap() == ip
}
