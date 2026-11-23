package checks

// One job, the way executor_task_handler.dart runs it between "job
// received" and POST /jobs/:id/result: modem check (bound_cellular), the
// operator's IP over the modem, «белые списки» measurement, the check
// itself, then provenance fields (vpn_active, whitelist, cellular_ip,
// traffic_bytes). The poll/heartbeat loop around it lives in the app.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/netbind"
)

// masker: the job's targets never reach env.Log (DiagLog.secret).
type masker struct {
	mu      sync.Mutex
	secrets []string
	env     Env
}

func (m *masker) secret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return
	}
	m.mu.Lock()
	for _, x := range m.secrets {
		if x == s {
			m.mu.Unlock()
			return
		}
	}
	m.secrets = append(m.secrets, s)
	sort.Slice(m.secrets, func(i, j int) bool { return len(m.secrets[i]) > len(m.secrets[j]) })
	m.mu.Unlock()
	if m.env.Secret != nil {
		m.env.Secret(s)
	}
}

func (m *masker) log(line string) {
	if m.env.Log == nil {
		return
	}
	m.mu.Lock()
	for _, s := range m.secrets {
		line = strings.ReplaceAll(line, s, "***")
	}
	m.mu.Unlock()
	m.env.Log(line)
}

var jsonAddrRe = regexp.MustCompile(`"(?:address|server|host|serverName|sni)"\s*:\s*"([^"]+)"`)

// hideResource = _hideResource: the resource, its host, addresses in a JSON
// config and any long query value are masked.
func (m *masker) hideResource(resource string) {
	m.secret(resource)
	if k := parseVpnURI(resource); k != nil && k.HasAddress {
		m.secret(k.Address)
	}
	for _, g := range jsonAddrRe.FindAllStringSubmatch(resource, -1) {
		m.secret(g[1])
	}
	raw := resource
	if !strings.Contains(raw, "://") {
		raw = "x://" + raw
	}
	if du, err := dartParseURI(raw); err == nil {
		m.secret(du.Host)
		if q, err := splitQuery(du.Query, lenientDecode); err == nil {
			for _, v := range q {
				if len([]rune(v)) >= 6 {
					m.secret(v)
				}
			}
		}
	}
}

// vpnActive: a VPN interface (tun/wg/tap...) is up on this computer --
// the desktop meaning of hasActiveVpn(). Container bridges are not VPNs.
func vpnActive(env Env) bool {
	list, err := netbind.List()
	if err != nil {
		return false
	}
	modem := ""
	if env.Net != nil {
		modem = env.Net.Iface().Name
	}
	for _, i := range list {
		if i.Kind != "virtual" || !i.Up || i.Name == modem || len(i.Addrs) == 0 || isContainerIface(i.Name) {
			continue
		}
		return true
	}
	return false
}

func isContainerIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "br-", "veth", "virbr", "lxc", "lxd", "cni", "flannel", "cali", "vxlan",
		"kube", "podman", "vmnet", "vboxnet", "dummy", "bond", "ifb"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	// Windows adapter names: Hyper-V/WSL switches, VMware/VirtualBox host-only.
	for _, p := range []string{"(wsl", "default switch", "hyper-v", "virtualbox", "vmware"} {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

func since(t time.Time) string { return fmt.Sprintf("%d мс", time.Since(t).Milliseconds()) }

func runImpl(ctx context.Context, env Env, job Job) (bool, map[string]any) {
	m := &masker{env: env}
	m.hideResource(job.Resource)
	jid := fmt.Sprintf("[задание %d]", job.ID)

	// Lease keepalive while the job runs (Android: Timer.periodic 5 s).
	aliveCtx, stopAlive := context.WithCancel(ctx)
	defer stopAlive()
	if env.Alive != nil {
		go func() {
			env.Alive()
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-aliveCtx.Done():
					return
				case <-t.C:
					env.Alive()
				}
			}
		}()
	}

	// bindCellular(waitMs: 5000): can we leave through the modem at all?
	pinned, why := modemUp(ctx, env, 5*time.Second)
	m.log(fmt.Sprintf("%s привязка к модему: %v%s", jid, pinned, func() string {
		if why != "" {
			return " (" + why + ")"
		}
		return ""
	}()))
	if !pinned {
		return false, map[string]any{
			"untestable":        true,
			"untestable_reason": "cellular_unavailable",
			"error":             why,
			"vpn_active":        vpnActive(env),
			"bound_cellular":    false,
		}
	}

	ipAt := time.Now()
	cellIP := cellularIPImpl(ctx, env)
	m.log(fmt.Sprintf("%s IP мобильной сети узнан за %s через %v%s", jid, since(ipAt), orDash(cellIP["via"]), func() string {
		if e, ok := cellIP["error"]; ok && e != nil {
			return fmt.Sprintf(" (не вышло: %v)", e)
		}
		return ""
	}()))
	rx0, tx0, cerr0 := env.Net.Counters()

	var whitelist map[string]any
	if len(job.WhitelistAllowed) > 0 && len(job.WhitelistControl) > 0 {
		wAt := time.Now()
		whitelist = whitelistProbeImpl(ctx, env, job.WhitelistAllowed, job.WhitelistControl)
		m.log(fmt.Sprintf("%s белые списки за %s: %s", jid, since(wAt), wlSummary(whitelist)))
	}
	if whitelist != nil && whitelist["verdict"] == "no_dns" {
		// The resolver died: nothing measured now would be about the
		// resource. Untestable (the customer is refunded); the caller reads
		// result.whitelist.verdict to report no_dns at once.
		return false, map[string]any{
			"untestable":        true,
			"untestable_reason": "dns_unavailable",
			"whitelist":         whitelist,
			"bound_cellular":    pinned,
		}
	}

	var result map[string]any
	var success bool
	checkAt := time.Now()
	m.log(fmt.Sprintf("%s проверка начата (%s%s)", jid, job.ResourceType, func() string {
		if job.IPFamily == 4 || job.IPFamily == 6 {
			return fmt.Sprintf(", IPv%d", job.IPFamily)
		}
		return ""
	}()))
	switch {
	case job.ResourceType == "vpn_key":
		o := checkVpnKey(ctx, env, job.Resource, job.ProbeSites, job.DownloadURL, job.NeedsCore, m.log)
		success = !o.untestable
		result = o.toJSON()
	case job.ResourceType == "subnet":
		untestable, r := checkSubnet(ctx, env, job.Resource)
		success = !untestable
		result = r
	case job.IPFamily == 6 && !modemHasGlobal6(env):
		// An IPv6 job, but the modem lost IPv6 since the last measurement
		// (3G, another APN): every connection would fail and read as a
		// block. Not testable here -- the customer is refunded; the caller
		// marks IPv6 as off at once.
		success = false
		result = map[string]any{"untestable": true, "untestable_reason": "ipv6_unavailable", "ip_family": 6}
		m.log(fmt.Sprintf("%s задание IPv6, но у модема нет IPv6", jid))
	default:
		result = checkIPOrDomain(ctx, env, job.Resource, job.IPFamily)
		success = true
		fam, _ := result["ip_family"].(int)
		result["probe"] = hostProbe(ctx, env, job.Resource, fam)
	}
	result["bound_cellular"] = pinned
	result["vpn_active"] = vpnActive(env)
	if whitelist != nil {
		result["whitelist"] = whitelist
	}
	result["cellular_ip"] = cellIP
	var traffic uint64
	if rx1, tx1, err := env.Net.Counters(); err == nil && cerr0 == nil && rx1+tx1 >= rx0+tx0 {
		traffic = rx1 + tx1 - (rx0 + tx0)
	}
	result["traffic_bytes"] = traffic

	for _, k := range []string{"resolved_ip", "final_host", "payload_url"} {
		if s, ok := result[k].(string); ok {
			m.secret(s)
		}
	}
	m.log(fmt.Sprintf("%s проверка за %s: успех=%v; %s; через модем=%v, VPN на компьютере=%v, трафик %d Б",
		jid, since(checkAt), success, summary(job.ResourceType, result), result["bound_cellular"], result["vpn_active"], traffic))
	return success, result
}

func orDash(v any) any {
	if v == nil {
		return "—"
	}
	return v
}

// summary = _summary: verdict fields only.
func summary(typ string, r map[string]any) string {
	v := func(k string) string {
		if r[k] == nil {
			return ""
		}
		return fmt.Sprintf("%s=%v ", k, r[k])
	}
	if typ == "vpn_key" {
		sites, _ := r["sites"].([]any)
		ok := 0
		var parts []string
		for _, x := range sites {
			s, _ := x.(map[string]any)
			if s == nil {
				continue
			}
			mark := "нет"
			if s["ok"] == true {
				ok++
				mark = "ок"
			}
			p := fmt.Sprintf("%v:%s", s["host"], mark)
			if s["http_code"] != nil {
				p += fmt.Sprintf("/%v", s["http_code"])
			}
			parts = append(parts, p)
		}
		dl := ""
		if d, ok := r["download"].(map[string]any); ok {
			dl = fmt.Sprintf("файл=%v %vМбит/с ", d["ok"], d["speed_mbps"])
		}
		return fmt.Sprintf("%s%s%sсайты %d/%d %s %s%s%s%s%s", v("works"), v("connected"), v("latency_ms"), ok, len(sites),
			strings.Join(parts, ","), dl, v("sockets_bound"), v("sockets_unbound"), v("untestable_reason"), v("error"))
	}
	if typ == "subnet" {
		return v("alive") + v("probed") + v("verdict") + v("untestable_reason") + v("error")
	}
	s := v("ip_family") + v("accessible") + v("tls_ok") + v("http_code") + v("time_ms") + v("tcp_latency_ms") + v("bytes") + v("block_reason") +
		v("anti_bot") + v("redirects") + v("payload_ok") + v("payload_bytes") + v("stall_bytes")
	if p, ok := r["probe"].(map[string]any); ok {
		s += fmt.Sprintf("пинг=%vмс tcp=%vмс ", p["icmp_ms"], p["tcp_ms"])
	}
	return s + v("error")
}

// wlSummary = _wlSummary (our control hosts only, no targets).
func wlSummary(w map[string]any) string {
	if w == nil {
		return "не замерялись"
	}
	side := func(a any) string {
		var parts []string
		list, _ := a.([]any)
		for _, x := range list {
			h, _ := x.(map[string]any)
			if h == nil {
				continue
			}
			mark := "нет"
			if h["ok"] == true {
				mark = "ок"
			}
			p := fmt.Sprintf("%v:%s", h["host"], mark)
			if h["tcp_ms"] != nil {
				p += fmt.Sprintf("/%vмс", h["tcp_ms"])
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, ",")
	}
	d, _ := w["dns"].(map[string]any)
	if d == nil {
		d = map[string]any{}
	}
	return fmt.Sprintf("вердикт=%v; из БС: %s; контрольные: %s; DNS %v (системный %v/%v, запасной %v, частный DNS %v)",
		w["verdict"], side(w["allowed"]), side(w["control"]), d["verdict"], d["system"], d["total"], d["fallback"], d["private_dns"])
}
