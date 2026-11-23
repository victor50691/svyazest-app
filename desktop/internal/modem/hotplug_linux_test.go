//go:build linux

package modem

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// SVYAZEST_HOTPLUG_TEST=1 go test -run Hotplug ./internal/modem (root: adds a dummy link).
func TestWatchHotplug(t *testing.T) {
	if os.Getenv("SVYAZEST_HOTPLUG_TEST") != "1" {
		t.Skip("set SVYAZEST_HOTPLUG_TEST=1 (needs root)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan struct{}, 16)
	done := make(chan error, 1)
	go func() { done <- WatchHotplug(ctx, func() { got <- struct{}{} }) }()
	time.Sleep(300 * time.Millisecond)
	if out, err := exec.Command("ip", "link", "add", "svztest0", "type", "dummy").CombinedOutput(); err != nil {
		t.Fatalf("ip link add: %v %s", err, out)
	}
	defer exec.Command("ip", "link", "del", "svztest0").Run()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no event for a new link")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchHotplug did not stop on cancel")
	}
}
