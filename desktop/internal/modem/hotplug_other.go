//go:build !linux

package modem

import "context"

// WatchHotplug: Linux only for now; elsewhere the 30-second round does it.
func WatchHotplug(ctx context.Context, kick func()) error {
	<-ctx.Done()
	return nil
}
