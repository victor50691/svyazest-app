//go:build linux

package modem

// WatchHotplug (1.0.4): the kernel tells us over rtnetlink when a network
// interface appears or goes away, changes state or gets an IPv4 address —
// a USB modem or a phone switched to «USB-модем» is then looked at at once
// instead of on the next 30-second round.

import (
	"context"
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

const iflaWireless = 11 // IFLA_WIRELESS: Wi-Fi scan chatter, not a new device

func WatchHotplug(ctx context.Context, kick func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR}
	if err := unix.Bind(fd, sa); err != nil {
		return err
	}
	// A blocked recvfrom is not woken by close(): wake up every second to
	// look at ctx instead.
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1})
	buf := make([]byte, 64<<10)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			switch {
			case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.ENOBUFS):
				kick() // events were lost: just look again
				continue
			}
			return err
		}
		if interesting(buf[:n]) {
			kick()
		}
	}
}

func interesting(b []byte) bool {
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		return true
	}
	for i := range msgs {
		m := &msgs[i]
		switch m.Header.Type {
		case unix.RTM_DELLINK, unix.RTM_NEWADDR, unix.RTM_DELADDR:
			return true
		case unix.RTM_NEWLINK:
			attrs, err := syscall.ParseNetlinkRouteAttr(m)
			if err != nil {
				return true
			}
			wireless := false
			for _, a := range attrs {
				if a.Attr.Type == iflaWireless {
					wireless = true
					break
				}
			}
			if !wireless {
				return true
			}
		}
	}
	return false
}
