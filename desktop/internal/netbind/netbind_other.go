//go:build !linux && !windows

package netbind

// Other OSes (macOS, BSD): not supported by the agent; these keep `go vet`
// and cross-builds of other packages working and fail honestly at runtime.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"
)

var errUnsupported = errors.New("netbind: эта ОС не поддерживается (только Linux и Windows)")

type osBinder struct{}

func listDetailedImpl(bool) ([]IfaceDetail, error) { return nil, errUnsupported }
func forInterfaceImpl(name string) (Binder, error) { return nil, errUnsupported }

func (b *binder) control(network, address string, c syscall.RawConn) error { return errUnsupported }
func (b *binder) localAddr(string, netip.Addr) (net.Addr, error)           { return nil, errUnsupported }
func (b *binder) Counters() (uint64, uint64, error)                        { return 0, 0, errUnsupported }
func (b *binder) Ping(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
	return 0, errUnsupported
}
