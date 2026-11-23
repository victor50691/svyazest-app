package api

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
)

// DeviceInfo collects what pair / auth/start / heartbeat say about this
// computer. fallbackID returns a random id kept in the data dir, used when
// the OS has no machine id (Docker without /etc/machine-id mounted).
func DeviceInfo(appVersion string, fallbackID func() string) DeviceMeta {
	model, osv, mid := platformInfo()
	mid = strings.TrimSpace(mid)
	if mid == "" && fallbackID != nil {
		mid = fallbackID()
	}
	if model == "" {
		model = "Компьютер"
	}
	return DeviceMeta{
		Platform:   PlatformName(),
		Model:      clip(model, 120),
		OSVersion:  clip(osv, 80),
		Arch:       archLabel(),
		AppVersion: appVersion,
		HWID:       HWID(mid),
	}
}

// HWID = sha256 hex of machine id + "svyazest" (64 chars, server HWID_RE).
func HWID(machineID string) string {
	if machineID == "" {
		return ""
	}
	h := sha256.Sum256([]byte(machineID + "svyazest"))
	return hex.EncodeToString(h[:])
}

func PlatformName() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "linux"
}

func archLabel() string {
	switch runtime.GOARCH {
	case "arm":
		return "armv7"
	case "386":
		return "x86"
	}
	return runtime.GOARCH
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		return string([]rune(s)[:n])
	}
	return s
}
