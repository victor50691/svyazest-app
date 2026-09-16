// Package xraylib embeds Xray-core into the «Связь Есть?» Android app as a
// gomobile library, so every socket Xray opens can be bound to the phone's
// cellular network by the Java side (android.net.Network.bindSocket) before
// it connects -- even while a third-party VPN is the default route.
//
// Uses only public Xray-core API: core.New / Instance.Start from
// github.com/xtls/xray-core/core, serial.LoadJSONConfig, and
// internet.RegisterDialerController (xray:api:beta), whose callback type is
// sing's control.Func: func(network, address string, conn syscall.RawConn) error.
package xraylib

import (
	"errors"
	"strings"
	"sync"
	"syscall"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	"github.com/xtls/xray-core/transport/internet"
)

// SocketBinder is implemented in Kotlin (NativePlugin.kt): bind the raw
// socket fd to the cellular Network. Returning false fails the dial, so a
// check never silently leaks through the VPN.
type SocketBinder interface {
	Bind(fd int) bool
}

var (
	instMu   sync.Mutex
	instance *core.Instance

	binderMu sync.RWMutex
	binder   SocketBinder

	regOnce sync.Once
	regErr  error
)

func controller(network, address string, conn syscall.RawConn) error {
	binderMu.RLock()
	b := binder
	binderMu.RUnlock()
	if b == nil {
		return nil
	}
	var bindErr error
	if err := conn.Control(func(fd uintptr) {
		if !b.Bind(int(fd)) {
			bindErr = errors.New("xraylib: could not bind socket to the cellular network")
		}
	}); err != nil {
		return err
	}
	return bindErr
}

// Start runs one Xray instance from a JSON config string, replacing any
// instance still running. Returns an error (Java: throws) on a bad config.
func Start(configJSON string, b SocketBinder) error {
	regOnce.Do(func() { regErr = internet.RegisterDialerController(controller) })
	if regErr != nil {
		return regErr
	}

	binderMu.Lock()
	binder = b
	binderMu.Unlock()

	instMu.Lock()
	defer instMu.Unlock()
	if instance != nil {
		_ = instance.Close()
		instance = nil
	}
	cfg, err := serial.LoadJSONConfig(strings.NewReader(configJSON))
	if err != nil {
		return err
	}
	inst, err := core.New(cfg)
	if err != nil {
		return err
	}
	if err := inst.Start(); err != nil {
		_ = inst.Close()
		return err
	}
	instance = inst
	return nil
}

// Stop shuts the running instance down (no-op when nothing runs).
func Stop() {
	instMu.Lock()
	defer instMu.Unlock()
	if instance != nil {
		_ = instance.Close()
		instance = nil
	}
	binderMu.Lock()
	binder = nil
	binderMu.Unlock()
}

// Version reports the embedded Xray-core version.
func Version() string {
	return core.Version()
}
