// Package checks: the actual measurements, ported 1:1 from the Android app
// (lib/services/checkers/*.dart, executor_task_handler.dart, NativePlugin.kt
// whitelistProbe/hostProbe/subnetProbe, build_xray_config.dart,
// vpn_uri_parser.dart, cellular_ip.dart). Owner: agent "checks".
//
// Result maps MUST carry the same keys/values the Android app sends: the
// server (routes/executor/index.js) and the DPI//Checker bot
// (telegram-bot device_check / _finalize_device_check) read them.
//
// CONTRACT:
package checks

import (
	"context"

	"svyazest-agent/internal/netbind"
)

// Job as handed out by GET /executor/v1/jobs/poll.
type Job struct {
	ID               int64    `json:"id"`
	CheckID          int64    `json:"check_id"`
	Resource         string   `json:"resource"`
	ResourceType     string   `json:"resource_type"` // vpn_key | ip | domain | subnet
	Protocol         string   `json:"protocol"`
	NeedsCore        string   `json:"needs_core"` // "next" = xray 26.10
	ProbeSites       []string `json:"probe_sites"`
	DownloadURL      string   `json:"download_url"`
	WhitelistAllowed []string `json:"whitelist_allowed"`
	WhitelistControl []string `json:"whitelist_control"`
	// IPFamily (1.0.1, Android 1.17.2 job field ip_family): 4 / 6 = an ip or
	// domain job goes over that IP family only (a domain by its A / AAAA from
	// the modem's DNS); 0 = as before.
	IPFamily int `json:"ip_family"`
}

type Env struct {
	Net          netbind.Binder // the modem; every check goes out through it
	XrayPath     string         // xray-core 26.3.27 binary
	XrayNextPath string         // xray-core 26.10.31 binary
	APIBase      string         // "https://dpichecker.st" (for /executor/v1/ip)
	DeviceToken  string
	Log          func(string) // diagnostic log line (never put targets in it)
	Secret       func(string) // register a string to be masked in diag logs
	// Alive is called periodically (~every 5 s) while a long check runs so
	// the caller can ping /jobs/:id/alive; may be nil.
	Alive func()
}

// Run performs one job. success/result exactly as the Android
// executor_task_handler sends them to POST /jobs/:id/result
// (status 'completed' when success, else 'failed'). It fills
// bound_cellular, whitelist, cellular_ip, traffic_bytes itself.
func Run(ctx context.Context, env Env, job Job) (success bool, result map[string]any) {
	return runImpl(ctx, env, job)
}

// WhitelistProbe = NativePlugin.whitelistProbe: returns the map with
// "verdict" (active|partial|inactive|no_internet|no_dns), "dns", hosts ...
func WhitelistProbe(ctx context.Context, env Env, allowed, control []string) map[string]any {
	return whitelistProbeImpl(ctx, env, allowed, control)
}

// CellularIP = lib/services/cellular_ip.dart (operator's public IP over the
// modem: our /executor/v1/ip, under whitelists Yandex's echo). {"ip","via","error"}.
func CellularIP(ctx context.Context, env Env) map[string]any { return cellularIPImpl(ctx, env) }

// XrayVersions: versions of both bundled cores for the UI ("26.3.27", "26.10.31").
func XrayVersions(env Env) (main, next string) { return xrayVersionsImpl(env) }

// IPv6Probe = executor_task_handler._measureIpv6 (Android 1.17.2): the
// modem has a global IPv6 address AND Yandex's IPv6-only echo
// (ipv6-internet.yandex.net, in every whitelist) answers over it. details:
// {"global": bool, "ip": the echoed IPv6 or nil, "error": text or nil,
// "why": a Russian line for the log}. Added in 1.0.1.
func IPv6Probe(ctx context.Context, env Env) (ok bool, details map[string]any) {
	return ipv6ProbeImpl(ctx, env)
}

// CellularEcho = ApiClient.cellularEcho(family) (Android 1.17.1): the
// operator's public IPv4 (family 4) or IPv6 (6) over the modem from
// ipv4-/ipv6-internet.yandex.net; ip "" = none, errText says why. 1.0.1.
func CellularEcho(ctx context.Context, env Env, family int) (ip string, errText string) {
	return cellularEchoImpl(ctx, env, family)
}

// VPNActive: a VPN interface is up on this computer (hasActiveVpn()), the
// vpn_active of results and of the network report. 1.0.1.
func VPNActive(env Env) bool { return vpnActive(env) }
