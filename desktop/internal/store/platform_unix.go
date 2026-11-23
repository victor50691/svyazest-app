//go:build !windows

package store

import (
	"os"
)

func platformDefaultDir() string {
	if InDocker() {
		if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
			return "/data"
		}
	}
	return "/var/lib/svyazest-agent"
}

// secureDir: 0700 (best effort -- a Docker bind mount may refuse chmod).
func secureDir(dir string) { _ = os.Chmod(dir, 0o700) }

// protect / unprotect: plain bytes in a 0600 file in a 0700 directory.
func protect(b []byte) ([]byte, error)   { return b, nil }
func unprotect(b []byte) ([]byte, error) { return b, nil }

// ProtectionLabel for the UI («Ещё → Устройство»).
func ProtectionLabel() string { return "файл 0600" }
