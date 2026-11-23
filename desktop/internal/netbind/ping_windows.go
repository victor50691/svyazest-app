//go:build windows

package netbind

// ICMP echo on Windows through the ICMP helper API (iphlpapi.dll): no raw
// sockets or admin rights needed. IcmpSendEcho2Ex / Icmp6SendEcho2 take a
// source address -- the interface's own -- which makes Windows send the
// echo out of that interface.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmp6CreateFile = iphlpapi.NewProc("Icmp6CreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
	procIcmp6SendEcho2  = iphlpapi.NewProc("Icmp6SendEcho2")
)

const ipSuccess = 0

func (b *binder) Ping(ctx context.Context, ip netip.Addr, timeout time.Duration) (time.Duration, error) {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return 0, errors.New("netbind: пустой адрес")
	}
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); timeout <= 0 || left < timeout {
			timeout = left
		}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	src, err := b.sourceAddr(ip)
	if err != nil {
		return 0, err
	}
	type res struct {
		d   time.Duration
		err error
	}
	ch := make(chan res, 1)
	// The call is synchronous (no event/APC) and returns after at most
	// `timeout`; run it aside so a cancelled ctx returns at once.
	go func() {
		d, err := icmpEcho(src, ip, timeout)
		ch <- res{d, err}
	}()
	select {
	case r := <-ch:
		return r.d, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func icmpEcho(src, dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	data := []byte("svyazest-agent-ping-0123456789ab")
	reply := make([]byte, 256+len(data)+64)
	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1
	}
	if dst.Is4() {
		h, _, e := procIcmpCreateFile.Call()
		if windows.Handle(h) == windows.InvalidHandle {
			return 0, fmt.Errorf("netbind: IcmpCreateFile: %w", e)
		}
		defer procIcmpCloseHandle.Call(h)
		// IPAddr is an in_addr: the four bytes in memory order.
		s4, d4 := src.As4(), dst.As4()
		start := time.Now()
		n, _, e := procIcmpSendEcho2Ex.Call(h, 0, 0, 0,
			uintptr(*(*uint32)(unsafe.Pointer(&s4[0]))),
			uintptr(*(*uint32)(unsafe.Pointer(&d4[0]))),
			uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0,
			uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(ms))
		rtt := time.Since(start)
		if n == 0 {
			return 0, fmt.Errorf("netbind: нет ответа на ping: %w", e)
		}
		// ICMP_ECHO_REPLY: Address u32, Status u32, RoundTripTime u32, ...
		addr := *(*[4]byte)(unsafe.Pointer(&reply[0]))
		status := *(*uint32)(unsafe.Pointer(&reply[4]))
		if status != ipSuccess {
			return 0, fmt.Errorf("netbind: ping: статус ICMP %d", status)
		}
		if netip.AddrFrom4(addr) != dst {
			return 0, fmt.Errorf("netbind: ping: ответ от %s", netip.AddrFrom4(addr))
		}
		return rtt, nil
	}
	h, _, e := procIcmp6CreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("netbind: Icmp6CreateFile: %w", e)
	}
	defer procIcmpCloseHandle.Call(h)
	ssa := windows.RawSockaddrInet6{Family: windows.AF_INET6, Addr: src.As16()}
	dsa := windows.RawSockaddrInet6{Family: windows.AF_INET6, Addr: dst.As16()}
	start := time.Now()
	n, _, e := procIcmp6SendEcho2.Call(h, 0, 0, 0,
		uintptr(unsafe.Pointer(&ssa)), uintptr(unsafe.Pointer(&dsa)),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(ms))
	rtt := time.Since(start)
	if n == 0 {
		return 0, fmt.Errorf("netbind: нет ответа на ping: %w", e)
	}
	// ICMPV6_ECHO_REPLY: IPV6_ADDRESS_EX (26 bytes, packed), Status u32 at 28.
	status := *(*uint32)(unsafe.Pointer(&reply[28]))
	if status != ipSuccess {
		return 0, fmt.Errorf("netbind: ping: статус ICMPv6 %d", status)
	}
	return rtt, nil
}
