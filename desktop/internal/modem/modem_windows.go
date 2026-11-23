//go:build windows

package modem

// Windows: built-in Mobile Broadband (WWAN) modems through netsh -- no
// admin rights needed for "show". USB sticks in HiLink/RNDIS mode are not
// WWAN devices and are found by Detect through netbind instead.

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const createNoWindow = 0x08000000

func platformDetect(ctx context.Context) []Info {
	out, err := run(ctx, 2*time.Second, "netsh", "mbn", "show", "interfaces")
	if err != nil {
		return nil
	}
	list := parseMbnInterfaces(decodeOEM(out))
	for i := range list {
		name := list[i].Interface
		if name == "" {
			continue
		}
		// homeprovider: the SIM's MCC+MNC; readyinfo: the IMSI as fallback.
		for _, what := range []string{"homeprovider", "readyinfo"} {
			// Raw command line: netsh wants interface="Cellular 2" with the
			// quotes around the value only, Go would quote the whole argument.
			o, err := runRaw(ctx, 1500*time.Millisecond, `netsh mbn show `+what+` interface="`+strings.ReplaceAll(name, `"`, ``)+`"`, "netsh", "mbn", "show", what)
			if err != nil {
				continue
			}
			if mm := parseMbnMCCMNC(decodeOEM(o)); mm != "" {
				list[i].MCCMNC = mm
				list[i].Country = CountryByMCC(mm[:3])
				break
			}
		}
	}
	return list
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	return runRaw(ctx, timeout, "", name, args...)
}

func runRaw(ctx context.Context, timeout time.Duration, cmdline, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	// A service has no console: without CREATE_NO_WINDOW every netsh call
	// would flash a window on an interactive desktop.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow, CmdLine: cmdline}
	return cmd.Output()
}
