package runner

// What the server learns about the modem's network (1.0.1, as the Android
// app 1.17.1/1.17.2):
//   - IPv6 (executor_task_handler._measureIpv6): every 10 minutes and on a
//     new modem -- a global IPv6 address on the modem and Yandex's
//     IPv6-only echo answering over it; sent as "ipv6" with every heartbeat,
//     only such devices get IPv6 «Белые списки» jobs.
//   - the network report (ApiClient.reportNetwork, POST /network-report):
//     both public addresses of the mobile network (Yandex's single-stack
//     echoes over the modem), operator, country, VPN -- for the moderator's
//     «Заявки». Once at start, then when the modem or one of the addresses
//     changes, never more often than every 10 minutes (the server keeps one
//     report a minute anyway).

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/modem"
	"svyazest-agent/internal/netbind"
)

const (
	ipv6Interval      = 10 * time.Minute
	netReportInterval = 10 * time.Minute
)

// measureNet: the 10-minute measurement (IPv6 + both echoes), then the
// network report if something changed.
func (r *Runner) measureNet(ctx context.Context, m modem.Info, b netbind.Binder) {
	if !r.ipv6At.IsZero() && r.ipv6Iface == m.Interface && time.Since(r.ipv6At) < ipv6Interval {
		return
	}
	r.ipv6At, r.ipv6Iface = time.Now(), m.Interface
	env := r.env(b)
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	var (
		wg        sync.WaitGroup
		v4, v4err string
		ok6       bool
		det       map[string]any
	)
	wg.Add(2)
	go func() { defer wg.Done(); v4, v4err = checks.CellularEcho(mctx, env, 4) }()
	go func() { defer wg.Done(); ok6, det = checks.IPv6Probe(mctx, env) }()
	wg.Wait()
	cancel()
	if ctx.Err() != nil {
		return
	}
	why, _ := det["why"].(string)
	if r.ipv6Ok == nil || *r.ipv6Ok != ok6 {
		r.j.Info("[IPv6] %s", why)
	} else {
		r.j.Debug("[IPv6] %s", why)
	}
	r.ipv6Ok = &ok6
	v6, _ := det["ip"].(string)
	v6err, _ := det["error"].(string)
	if v6 != "" {
		v6err = ""
	}
	r.networkReport(ctx, m, b, env, v4, v4err, v6, v6err)
}

// networkReport = ApiClient.reportNetwork: over the modem (via=cellular,
// the server records the request's address too); if that fails, the
// normal way (via=default, with the error). Async: the cycle goes on.
func (r *Runner) networkReport(ctx context.Context, m modem.Info, b netbind.Binder, env checks.Env, v4, v4err, v6, v6err string) {
	key := m.Interface + "|" + v4 + "|" + v6
	if key == r.netReportedKey.Load().(string) {
		return
	}
	if !r.netReportAt.IsZero() && time.Since(r.netReportAt) < netReportInterval {
		return // changed too soon: the next measurement (10 min) sends it
	}
	r.netReportAt = time.Now()
	body := map[string]any{
		"operator_name":   nilIfEmpty(m.Operator),
		"network_country": nilIfEmpty(m.Country), // by the modem's MCC
		"sim_country":     nilIfEmpty(m.Country),
		"vpn_active":      checks.VPNActive(env),
	}
	if v4 != "" {
		body["ipv4"] = v4
	}
	if v6 != "" {
		body["ipv6"] = v6
	}
	var ipErr []string
	if v4err != "" {
		ipErr = append(ipErr, "ipv4: "+v4err)
	}
	if v6err != "" {
		ipErr = append(ipErr, "ipv6: "+v6err)
	}
	if len(ipErr) > 0 {
		body["ip_error"] = joinSemi(ipErr)
	}
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := r.api.NetworkReport(rctx, b.HTTPClient(15*time.Second), body)
		if err == nil {
			r.netReportedKey.Store(key)
			r.j.Debug("отчёт о сети отправлен через модем %s (IPv4 %s, IPv6 %s)", m.Interface, yesNo(v4 != ""), yesNo(v6 != ""))
			return
		}
		body["error"] = shortErr(unwrapURL(err))
		if err2 := r.api.NetworkReport(rctx, nil, body); err2 == nil {
			// The server has the addresses (from the echoes) -- not resent
			// until something changes.
			r.netReportedKey.Store(key)
			r.j.Debug("отчёт о сети через модем не ушёл (%s), отправлен обычным путём", shortErr(unwrapURL(err)))
		} else {
			r.j.Debug("отчёт о сети не отправлен: через модем — %s, обычным путём — %s", shortErr(err), shortErr(err2))
		}
	}()
}

// unwrapURL: the error without Go's `Post "https://…":` prefix.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "есть"
	}
	return "нет"
}

func joinSemi(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}
