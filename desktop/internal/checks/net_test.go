package checks

// Against real hosts through testBinder (the build server's own network).
// SVZ_NET_TESTS=1 to run.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"
)

func needNet(t *testing.T) {
	if os.Getenv("SVZ_NET_TESTS") == "" {
		t.Skip("SVZ_NET_TESTS not set")
	}
}

func dump(t *testing.T, name string, v any) {
	b, _ := json.Marshal(v)
	t.Logf("%s: %s", name, b)
}

func testEnv(b *testBinder) Env {
	return Env{Net: b, Log: func(s string) {}, Secret: func(string) {}}
}

var wlAllowed = []string{"ya.ru", "vk.ru", "mail.ru", "nalog.gov.ru"}
var wlControl = []string{"google.com", "wikipedia.org", "cloudflare.com"}

func TestWhitelistProbeReal(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	w := WhitelistProbe(context.Background(), env, wlAllowed, wlControl)
	dump(t, "whitelist", w)
	if w["verdict"] != "inactive" && w["verdict"] != "partial" {
		t.Errorf("verdict %v (a foreign server is not under whitelists)", w["verdict"])
	}
	d := w["dns"].(map[string]any)
	if d["verdict"] != "ok" || d["system"].(int) != 7 || d["total"].(int) != 7 {
		t.Errorf("dns %v", d)
	}
	for _, h := range w["control"].([]any) {
		m := h.(map[string]any)
		if m["ok"] != true || m["dns"] != "system" || m["tcp_ms"] == nil {
			t.Errorf("control host %v", m)
		}
	}
	// No allowed/control lists: "unknown" (Android: allowed.isEmpty -> unknown).
	if v := WhitelistProbe(context.Background(), env, nil, wlControl)["verdict"]; v != "unknown" {
		t.Errorf("empty allowed -> %v", v)
	}
	// No modem at all.
	if v := WhitelistProbe(context.Background(), Env{}, wlAllowed, wlControl)["verdict"]; v != "no_cellular" {
		t.Errorf("no binder -> %v", v)
	}
}

// The system resolver is dead but 77.88.8.8 answers directly: no_dns.
func TestWhitelistNoDNS(t *testing.T) {
	needNet(t)
	b := newTestBinder(nil)
	b.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) { return nil, errNoDNS }
	w := WhitelistProbe(context.Background(), testEnv(b), wlAllowed, wlControl)
	dump(t, "whitelist(no dns)", w)
	d := w["dns"].(map[string]any)
	if w["verdict"] != "no_dns" || d["verdict"] != "system_down" || d["fallback"].(int) == 0 {
		t.Errorf("verdict %v dns %v", w["verdict"], d)
	}
	for _, h := range w["allowed"].([]any) {
		m := h.(map[string]any)
		if m["ip"] != nil && m["dns"] != "fallback" {
			t.Errorf("host %v", m)
		}
	}
	// And a Run under it: dns_unavailable, untestable.
	ok, r := Run(context.Background(), testEnv(b), Job{ID: 1, Resource: "example.com", ResourceType: "domain",
		WhitelistAllowed: wlAllowed, WhitelistControl: wlControl})
	dump(t, "run(no dns)", r)
	if ok || r["untestable_reason"] != "dns_unavailable" || r["bound_cellular"] != true || r["whitelist"] == nil {
		t.Errorf("run: ok=%v %v", ok, r)
	}
}

func TestDNSQueryA(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	a, ok := dnsQueryA(context.Background(), env, fallbackDNS, "ya.ru", 3*time.Second)
	if !ok || !a.Is4() {
		t.Fatalf("dnsQueryA: %v %v", a, ok)
	}
	if _, ok := dnsQueryA(context.Background(), env, fallbackDNS, "no-such-name.invalid", 3*time.Second); ok {
		t.Error("NXDOMAIN must not answer")
	}
}

func TestHostProbe(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	for _, h := range []string{"1.1.1.1", "example.com"} {
		p := hostProbe(context.Background(), env, h, 4)
		dump(t, "probe "+h, p)
		if p["icmp_ms"] == nil || p["tcp_ms"] == nil || p["error"] != nil {
			t.Errorf("%s: %v", h, p)
		}
	}
	if p := hostProbe(context.Background(), env, "no-such-name.invalid", 4); p["error"] != "dns" {
		t.Errorf("nx: %v", p)
	}
}

func TestCheckDomains(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	cases := []struct {
		domain     string
		accessible bool
		block      any
	}{
		{"example.com", true, nil},
		{"wikipedia.org", true, nil},
		{"github.com", true, nil},
		{"self-signed.badssl.com", false, "tls"},
		{"expired.badssl.com", false, "tls"},
		{"no-such-name.invalid", false, "dns"},
		{"localtest.me", false, "dns_poisoned"}, // resolves to 127.0.0.1
	}
	for _, c := range cases {
		r := checkIPOrDomain(context.Background(), env, c.domain, 0)
		dump(t, c.domain, r)
		if r["accessible"] != c.accessible || r["block_reason"] != c.block {
			t.Errorf("%s: accessible=%v block=%v error=%v", c.domain, r["accessible"], r["block_reason"], r["error"])
		}
		if len(r) != 22 {
			t.Errorf("%s: %d keys, want all 22 of IpCheckResult.toJson (1.17.2)", c.domain, len(r))
		}
	}
}

func TestCheckIP(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	r := checkIPOrDomain(context.Background(), env, "1.1.1.1", 0)
	dump(t, "1.1.1.1", r)
	if r["accessible"] != true || r["port_443_open"] != true || r["tls_ok"] != true || r["port_80_open"] != true || r["resolved_ip"] != "1.1.1.1" {
		t.Errorf("1.1.1.1: %v", r)
	}
	r = checkIPOrDomain(context.Background(), env, "192.0.2.1", 0) // TEST-NET-1: nothing answers
	dump(t, "192.0.2.1", r)
	if r["accessible"] != false || r["error"] == nil || r["tcp_latency_ms"] != nil {
		t.Errorf("192.0.2.1: %v", r)
	}
}

func TestSubnet(t *testing.T) {
	needNet(t)
	env := testEnv(newTestBinder(nil))
	untestable, r := checkSubnet(context.Background(), env, "1.1.1.0/24")
	hosts := r["hosts"]
	r2 := copyMap(r)
	delete(r2, "hosts")
	dump(t, "1.1.1.0/24", r2)
	if untestable || r["probed"].(int) != 256 || r["alive"].(int) == 0 || r["verdict"] == "unknown" || r["works"] != true {
		t.Errorf("subnet: %v", r2)
	}
	if n := len(hosts.([]any)); n == 0 || n > 40 {
		t.Errorf("hosts %d", n)
	}
	if u, r := checkSubnet(context.Background(), env, "garbage"); !u || r["untestable_reason"] != "bad_cidr" {
		t.Errorf("bad cidr: %v", r)
	}
	if u, r := checkSubnet(context.Background(), Env{}, "1.1.1.0/24"); !u || r["untestable_reason"] != "cellular_unavailable" {
		t.Errorf("no modem: %v", r)
	}
}

func TestCellularIP(t *testing.T) {
	needNet(t)
	var gotAuth, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/executor/v1/ip" {
			http.NotFound(w, r)
			return
		}
		gotAuth, gotVer = r.Header.Get("Authorization"), r.Header.Get("X-App-Version")
		w.Write([]byte(`{"ip":"203.0.113.7"}`))
	}))
	defer srv.Close()
	resetCellIP()
	env := testEnv(newTestBinder(nil))
	env.APIBase, env.DeviceToken = srv.URL, "tok"
	r := CellularIP(context.Background(), env)
	if r["ip"] != "203.0.113.7" || r["via"] != "dpichecker" || gotAuth != "Bearer tok" || gotVer != AppVersion {
		t.Errorf("ours: %v auth=%q ver=%q", r, gotAuth, gotVer)
	}
	// Our server down -> Yandex's echo.
	resetCellIP()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer bad.Close()
	env.APIBase = bad.URL
	r = CellularIP(context.Background(), env)
	dump(t, "cellular ip via yandex", r)
	if r["via"] != "yandex" || r["ip"] == nil {
		t.Errorf("yandex: %v", r)
	}
	resetCellIP()
}

func resetCellIP() {
	cellIPCache.Lock()
	cellIPCache.last = nil
	cellIPCache.Unlock()
}

func TestRunDomainAndIP(t *testing.T) {
	needNet(t)
	resetCellIP()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ip":"203.0.113.9"}`)) }))
	defer srv.Close()
	b := newTestBinder(nil)
	var logs []string
	alive := 0
	env := Env{Net: b, APIBase: srv.URL, Log: func(s string) { logs = append(logs, s) }, Secret: func(string) {}, Alive: func() { alive++ }}
	ok, r := Run(context.Background(), env, Job{ID: 7, Resource: "wikipedia.org", ResourceType: "domain",
		WhitelistAllowed: wlAllowed, WhitelistControl: wlControl})
	dump(t, "run domain", r)
	if !ok || r["accessible"] != true || r["bound_cellular"] != true || r["vpn_active"] != false ||
		r["whitelist"] == nil || r["cellular_ip"] == nil || r["probe"] == nil {
		t.Errorf("run: %v", r)
	}
	if _, isU := r["traffic_bytes"].(uint64); !isU || r["traffic_bytes"].(uint64) == 0 {
		t.Errorf("traffic_bytes %v", r["traffic_bytes"])
	}
	if alive == 0 {
		t.Error("Alive never called")
	}
	for _, l := range logs {
		if contains(l, "wikipedia.org") {
			t.Errorf("target in log: %s", l)
		}
	}
	// The JSON must round-trip like the app would POST it.
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	ok, r = Run(context.Background(), env, Job{ID: 8, Resource: "1.1.1.1", ResourceType: "ip"})
	dump(t, "run ip", r)
	if !ok || r["accessible"] != true {
		t.Errorf("run ip: %v", r)
	}
	// No modem: cellular_unavailable, as Android's bind failure.
	ok, r = Run(context.Background(), Env{}, Job{ID: 9, Resource: "1.1.1.1", ResourceType: "ip"})
	if ok || r["untestable_reason"] != "cellular_unavailable" || r["bound_cellular"] != false {
		t.Errorf("no modem: %v", r)
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
