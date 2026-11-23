//go:build !linux && !windows

package modem

import "context"

// Other OSes: no modem stack is asked; Detect still reports HiLink/ZTE and
// tethering where netbind works (it does not there either -- nothing found).
func platformDetect(ctx context.Context) []Info { return nil }
