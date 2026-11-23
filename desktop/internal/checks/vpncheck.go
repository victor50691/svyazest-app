package checks

// Port of lib/services/checkers/vpn_checker.dart: the key through xray,
// then real sites and the ~1 MB file through the tunnel.

import (
	"context"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// vpnResult = VpnCheckResult.toJson.
type vpnOutcome struct {
	connected        bool
	latencyMs        any
	err              any
	untestable       bool
	untestableReason any
	socketsBound     int
	socketsUnbound   int
	sites            []map[string]any
	download         map[string]any
	bindError        any
	core             string
}

func (o *vpnOutcome) works() bool {
	if !o.connected {
		return false
	}
	if len(o.sites) > 0 {
		any := false
		for _, s := range o.sites {
			if s["ok"] == true {
				any = true
			}
		}
		if !any {
			return false
		}
	}
	return o.download == nil || o.download["ok"] == true
}

func (o *vpnOutcome) toJSON() map[string]any {
	sites := toAnyList(o.sites)
	m := map[string]any{
		"connected":         o.connected,
		"latency_ms":        o.latencyMs,
		"error":             o.err,
		"untestable":        o.untestable,
		"untestable_reason": o.untestableReason,
		"sockets_bound":     o.socketsBound,
		"sockets_unbound":   o.socketsUnbound,
		"works":             o.works(),
		"sites":             sites,
		"xray_core":         "26.3",
	}
	if o.core == "next" {
		m["xray_core"] = "26.10"
	}
	if o.download != nil {
		m["download"] = o.download
	}
	if o.bindError != nil {
		m["bind_error"] = o.bindError
	}
	return m
}

func checkVpnKey(ctx context.Context, env Env, uri string, probeSites []string, downloadURL, core string, logf func(string)) *vpnOutcome {
	key := parseVpnURI(uri)
	if key == nil {
		return &vpnOutcome{untestable: true, untestableReason: "unrecognized_scheme", core: core}
	}
	if !key.supported() {
		reason := key.Unsupported
		if reason == "" {
			reason = "missing_fields"
		}
		return &vpnOutcome{untestable: true, untestableReason: reason, core: core}
	}
	out := &vpnOutcome{core: core}
	port, err := freeLocalPort()
	if err != nil {
		out.err = "local proxy did not come up in time"
		return out
	}
	cfg, err := buildXrayConfig(key, port)
	if err != nil {
		out.err = err.Error()
		return out
	}
	var sockopt map[string]any
	dnsServers := modemDNS(env)
	if env.Net != nil {
		sockopt = env.Net.XraySockopt()
	}
	desktopXrayConfig(cfg, sockopt, dnsServers)

	bin, label := env.XrayPath, "26.3"
	if core == "next" {
		bin, label = env.XrayNextPath, "26.10"
	}
	var attempts atomic.Int32
	proc, serr := startXray(bin, marshalConfig(cfg), logf)
	switch {
	case serr != nil:
		out.err = "xray: xray " + label + " не найден в программе"
		if logf != nil {
			logf("ядро " + label + " не запустилось: " + shortErr(serr))
		}
	case !proc.waitReady(ctx, port, 5*time.Second):
		if proc.exited() {
			msg := proc.lastLine()
			if core == "next" {
				msg = trimSpace("xray 26.10 exited (" + strconv.Itoa(proc.exitCode()) + ") " + msg)
			} else if msg == "" {
				msg = "xray exited (" + strconv.Itoa(proc.exitCode()) + ")"
			}
			out.err = "xray: " + msg
			if logf != nil {
				logf("ядро " + label + " не запустилось: код " + strconv.Itoa(proc.exitCode()))
			}
		} else {
			out.err = "local proxy did not come up in time"
		}
	default:
		if logf != nil {
			logf("ядро " + label + " запущено отдельным процессом, исходящие привязаны к модему")
		}
		start := time.Now()
		attempts.Add(1)
		probe := probeThroughProxy(ctx, port)
		lat := time.Since(start).Milliseconds()
		if !probe {
			out.err = "target unreachable through this key"
		} else {
			sites := []map[string]any{}
			for _, host := range probeSites {
				attempts.Add(1)
				sites = append(sites, fetchThroughProxy(ctx, port, &url.URL{Scheme: "https", Host: host, Path: "/"}, host))
			}
			var dl map[string]any
			if downloadURL != "" {
				if u, err := parseRef(downloadURL); err == nil {
					attempts.Add(1)
					dl = downloadThroughProxy(ctx, port, u)
				} else {
					dl = map[string]any{"ok": false, "http_code": nil, "bytes": 0, "expected_bytes": nil, "time_ms": 0, "speed_mbps": 0, "error": "FormatException"}
				}
			}
			out.connected, out.latencyMs, out.sites, out.download = true, lat, sites, dl
		}
	}
	if proc != nil {
		proc.stop()
		if logf != nil {
			logf("ядро " + label + " остановлено")
		}
	}
	if out.sites == nil {
		out.sites = []map[string]any{}
	}
	n := int(attempts.Load())
	started := serr == nil && n > 0
	switch {
	case !started:
		// Never came up: like XrayNext.stop, the 26.10 process reports one
		// unbound start with its error; the embedded core opened nothing.
		if core == "next" {
			out.socketsUnbound = 1
			out.bindError = out.err
		}
	case len(sockopt) == 0:
		// Nothing pinned xray to the modem: what it opened went out of the
		// default route (Android's "unbound").
		out.socketsUnbound = n
		out.bindError = "no interface binding for xray"
	default:
		// Every outbound carries the modem's sockopt; xray fails a dial it
		// cannot bind rather than leaving another way.
		out.socketsBound = n
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// modemDNS: the resolvers LookupHost uses (normally the modem's own,
// DHCP/bearer) -- first in xray's DNS list, queried out of the modem.
func modemDNS(env Env) []netip.Addr {
	if env.Net == nil {
		return nil
	}
	servers, _ := env.Net.DNSInfo()
	return servers
}

func probeThroughProxy(ctx context.Context, port int) bool {
	tr := proxyTransport(port, 12*time.Second, 12*time.Second, 12*time.Second)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: dartRedirectPolicy(5)}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "GET", "https://www.gstatic.com/generate_204", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if _, err := ioCopyDiscard(resp.Body); err != nil {
		return false
	}
	return resp.StatusCode < 500
}

// fetchThroughProxy: a site through the tunnel, whole body (capped at 2 MB);
// ok only if real content came back.
func fetchThroughProxy(ctx context.Context, port int, u *url.URL, host string) map[string]any {
	tr := proxyTransport(port, 10*time.Second, 12*time.Second, 15*time.Second)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: dartRedirectPolicy(5)}
	start := time.Now()
	nbytes := 0
	var code any
	var errName any
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", u.String(), nil)
	if err == nil {
		req.Header.Set("User-Agent", androidUA)
		hdr := time.AfterFunc(12*time.Second+15*time.Second, cancel)
		resp, derr := client.Do(req)
		hdrExpired := !hdr.Stop()
		if derr != nil {
			if hdrExpired {
				derr = &phaseErr{"timeout", derr}
			}
			errName = dartErrType(derr)
		} else {
			code = resp.StatusCode
			rerr := newIdleReader(cancel, 15*time.Second).read(resp.Body, func(b []byte) bool {
				nbytes += len(b)
				return nbytes <= 2*1024*1024
			})
			resp.Body.Close()
			if rerr != nil {
				errName = dartErrType(rerr)
			}
		}
	} else {
		errName = "FormatException"
	}
	ok := false
	if c, isInt := code.(int); isInt {
		ok = c < 400 && nbytes > 0
	}
	m := map[string]any{"host": host, "ok": ok, "http_code": code, "time_ms": time.Since(start).Milliseconds(), "bytes": nbytes}
	if errName != nil {
		m["error"] = errName
	}
	return m
}

// downloadThroughProxy: the fixed file; complete = ok, a transfer that stops
// after the first tens of KB is the DPI cut signature.
func downloadThroughProxy(ctx context.Context, port int, u *url.URL) map[string]any {
	tr := proxyTransport(port, 10*time.Second, 12*time.Second, 15*time.Second)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: dartRedirectPolicy(5)}
	start := time.Now()
	nbytes := 0
	var code, expected, errName any
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", u.String(), nil)
	if err == nil {
		req.Header.Set("User-Agent", androidUA)
		hdr := time.AfterFunc(12*time.Second+15*time.Second, cancel)
		resp, derr := client.Do(req)
		hdrExpired := !hdr.Stop()
		if derr != nil {
			if hdrExpired {
				derr = &phaseErr{"timeout", derr}
			}
			errName = dartErrType(derr)
		} else {
			code = resp.StatusCode
			if resp.ContentLength > 0 {
				expected = int(resp.ContentLength)
			}
			rerr := newIdleReader(cancel, 20*time.Second).read(resp.Body, func(b []byte) bool {
				nbytes += len(b)
				return time.Since(start) <= 40*time.Second
			})
			resp.Body.Close()
			if rerr != nil {
				errName = dartErrType(rerr)
			}
		}
	} else {
		errName = "FormatException"
	}
	elapsed := time.Since(start)
	nominal := 1000000
	if e, ok := expected.(int); ok {
		nominal = e
	}
	ok := false
	if c, isInt := code.(int); isInt {
		ok = c < 400 && nbytes >= int(math.Floor(float64(nominal)*0.9))
	}
	secs := float64(elapsed.Milliseconds()) / 1000.0
	var speed any = 0
	if secs > 0 {
		speed = math.Round(float64(nbytes)*8/secs/1e6*100) / 100
	}
	m := map[string]any{"ok": ok, "http_code": code, "bytes": nbytes, "expected_bytes": expected,
		"time_ms": elapsed.Milliseconds(), "speed_mbps": speed}
	if errName != nil {
		m["error"] = errName
	}
	return m
}
