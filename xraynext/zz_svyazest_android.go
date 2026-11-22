//go:build android

// «Связь Есть?» (2026-11-21): this build of Xray-core runs as a separate
// process next to the embedded 26.3.27 (xraylib.aar). The app passes the
// cellular network handle (android.net.Network.getNetworkHandle) in
// SVZ_NET_HANDLE; before anything dials, the whole process is pinned to that
// network -- sockets and DNS -- regardless of the default route or any VPN.
// If pinning fails the process exits: a check must never leak elsewhere.
package main

/*
#cgo LDFLAGS: -landroid
#include <android/multinetwork.h>
#include <errno.h>
static int svz_pin(unsigned long long h) {
	if (android_setprocnetwork((net_handle_t)h) != 0) return errno ? errno : -1;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
	"strconv"
)

func init() {
	s := os.Getenv("SVZ_NET_HANDLE")
	if s == "" {
		return
	}
	h, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "svyazest: bad SVZ_NET_HANDLE")
		os.Exit(97)
	}
	if rc := C.svz_pin(C.ulonglong(h)); rc != 0 {
		fmt.Fprintf(os.Stderr, "svyazest: android_setprocnetwork failed: %d\n", int(rc))
		os.Exit(97)
	}
}
