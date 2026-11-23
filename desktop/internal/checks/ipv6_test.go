package checks

// IPv6 «Белые списки» jobs (ip_family, 1.0.1 = Android 1.17.2) and the
// IPv6 measurement. Offline tests run always; the real-network ones need
// SVZ_NET_TESTS=1 and a host with working IPv6 (the build server's eth0).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"svyazest-agent/internal/netbind"
)

func TestIPv6Helpers(t *testing.T) {
	for s, want := range map[string]bool{
		"2606:4700:4700::1111": true, "[2a02:6b8::2:242]": true, "fe80::1%eth0": false,
		"1.1.1.1": false, "example.com": false, "::ffff:1.2.3.4": true, "2a02:6b8::zz": false,
	} {
		if _, ok := asLiteralIPv6(s); ok != want {
			t.Errorf("asLiteralIPv6(%q) = %v", s, ok)
		}
	}
	for s, want := range map[string]bool{
		"::": true, "::1": true, "fe80::1": true, "ff02::1": true, "fd00::1": true, "fc12::1": true,
		"::ffff:1.2.3.4": true, "2a02:6b8::2:242": false, "64:ff9b::102:304": false,
	} {
		if got := isBogusIP6(netip.MustParseAddr(s)); got != want {
			t.Errorf("isBogusIP6(%s) = %v", s, got)
		}
	}
	if !isNat64(netip.MustParseAddr("64:ff9b::102:304")) || isNat64(netip.MustParseAddr("2a02:6b8::5d7")) {
		t.Error("isNat64")
	}
}

// A fake DNS answering both families plus a NAT64 address.
func dualLookup(v4, v6 []string) func(ctx context.Context, host string) ([]netip.Addr, error) {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, s := range append(append([]string{}, v4...), v6...) {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestLookupFamilyFilters(t *testing.T) {
	b := newTestBinder(nil)
	b.lookup = dualLookup([]string{"192.0.2.10"}, []string{"64:ff9b::c000:20a", "2001:db8::10"})
	env := testEnv(b)
	v6, err := lookupFamily(context.Background(), env, "x.test", 6, 0)
	if err != nil || len(v6) != 1 || v6[0].String() != "2001:db8::10" {
		t.Errorf("v6 = %v %v (NAT64 must be dropped)", v6, err)
	}
	v4, _ := lookupFamily(context.Background(), env, "x.test", 4, 0)
	if len(v4) != 1 || v4[0].String() != "192.0.2.10" {
		t.Errorf("v4 = %v", v4)
	}
	if l, _ := lookupFamily(context.Background(), env, "2001:db8::1", 4, 0); len(l) != 0 {
		t.Errorf("v6 literal asked as v4: %v", l)
	}
}

// Only A records (or only NAT64 AAAA): an IPv6 job reads no_aaaa, not a block.
func TestDomainNoAAAA(t *testing.T) {
	b := newTestBinder(nil)
	b.lookup = dualLookup([]string{"192.0.2.10"}, []string{"64:ff9b::c000:20a"})
	r := checkIPOrDomain(context.Background(), testEnv(b), "v4only.test", 6)
	if r["block_reason"] != "no_aaaa" || r["error"] != "no AAAA" || r["ip_family"] != 6 || r["accessible"] != false || len(r) != 22 {
		t.Errorf("no AAAA: %v", r)
	}
	b.lookup = dualLookup(nil, []string{"fd00::5"})
	r = checkIPOrDomain(context.Background(), testEnv(b), "spoof.test", 6)
	if r["block_reason"] != "dns_poisoned" || r["resolved_ip"] != "fd00::5" || r["ip_family"] != 6 {
		t.Errorf("bogus AAAA: %v", r)
	}
	b.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) { return nil, errNoDNS }
	r = checkIPOrDomain(context.Background(), testEnv(b), "dead.test", 4)
	if r["block_reason"] != "dns" || r["ip_family"] != 4 {
		t.Errorf("dns failed (v4): %v", r)
	}
	r = checkIPOrDomain(context.Background(), testEnv(b), "dead.test", 0)
	if r["block_reason"] != "dns" || r["ip_family"] != nil {
		t.Errorf("dns failed (as before): %v", r)
	}
}

// The modem has no global IPv6 (loopback stands in): an ip_family=6 job
// is untestable with ipv6_unavailable, like Android 1.17.2.
func TestRunIPv6Unavailable(t *testing.T) {
	b := newTestBinder(nil)
	b.iface.Name = "lo"
	b.iface.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ip":"203.0.113.9"}`)) }))
	defer srv.Close()
	env := testEnv(b)
	env.APIBase = srv.URL
	if modemHasGlobal6(env) {
		t.Fatal("lo has a global IPv6?")
	}
	ok, d := IPv6Probe(context.Background(), env)
	if ok || d["global"] != false || d["why"] == nil {
		t.Errorf("IPv6Probe on lo: %v %v", ok, d)
	}
	success, r := Run(context.Background(), env, Job{ID: 1, Resource: "2606:4700:4700::1111", ResourceType: "ip", IPFamily: 6})
	dump(t, "run v6 on lo", r)
	if success || r["untestable"] != true || r["untestable_reason"] != "ipv6_unavailable" || r["ip_family"] != 6 ||
		r["bound_cellular"] != true || r["cellular_ip"] == nil {
		t.Errorf("ipv6 job without IPv6: success=%v %v", success, r)
	}
}

// ---- real network (build server: eth0 with a global IPv6) ----

func realBinder(t *testing.T) netbind.Binder {
	b, err := netbind.ForInterface(defaultIface())
	if err != nil || b == nil {
		t.Fatalf("netbind.ForInterface(%s): %v", defaultIface(), err)
	}
	return b
}

func binders(t *testing.T) map[string]netbind.Binder {
	return map[string]netbind.Binder{"test": newTestBinder(nil), "netbind": realBinder(t)}
}

func TestIPv6ProbeReal(t *testing.T) {
	needNet(t)
	for name, b := range binders(t) {
		env := Env{Net: b, Log: func(string) {}, Secret: func(string) {}}
		ok, d := IPv6Probe(context.Background(), env)
		dump(t, name+" IPv6Probe", d)
		ip, _ := d["ip"].(string)
		if !ok || d["global"] != true || !strings.Contains(ip, ":") {
			t.Errorf("%s: IPv6Probe = %v %v", name, ok, d)
		}
		v4, e4 := CellularEcho(context.Background(), env, 4)
		v6, e6 := CellularEcho(context.Background(), env, 6)
		t.Logf("%s: echo v4=%q (%s) v6=%q (%s)", name, v4, e4, v6, e6)
		if v4 == "" || strings.Contains(v4, ":") || !strings.Contains(v6, ":") {
			t.Errorf("%s: echo v4=%q %s v6=%q %s", name, v4, e4, v6, e6)
		}
	}
}

func TestIPFamilyJobsReal(t *testing.T) {
	needNet(t)
	for name, b := range binders(t) {
		env := Env{Net: b, APIBase: "http://127.0.0.1:1", Log: func(string) {}, Secret: func(string) {}}
		for _, c := range []struct {
			target string
			fam    int
			acc    bool
			block  any
			v6     bool
		}{
			{"ya.ru", 6, true, nil, true},
			{"google.com", 6, true, nil, true},
			{"example.com", 4, true, nil, false},
			{"github.com", 6, false, "no_aaaa", false},
			{"ipv6-internet.yandex.net", 4, false, "dns", false}, // AAAA only
			{"2606:4700:4700::1111", 6, true, nil, true},
			{"[2a02:6b8::2:242]", 6, true, nil, true},
			{"1.1.1.1", 4, true, nil, false},
		} {
			success, r := Run(context.Background(), env, Job{ID: 7, Resource: c.target, ResourceType: "ip", IPFamily: c.fam})
			dump(t, name+" "+c.target, r)
			rip, _ := r["resolved_ip"].(string)
			if !success || r["accessible"] != c.acc || r["block_reason"] != c.block || r["ip_family"] != c.fam ||
				(c.acc && strings.Contains(rip, ":") != c.v6) {
				t.Errorf("%s %s/%d: success=%v accessible=%v block=%v ip=%v fam=%v err=%v", name, c.target, c.fam, success,
					r["accessible"], r["block_reason"], r["resolved_ip"], r["ip_family"], r["error"])
			}
			p, _ := r["probe"].(map[string]any)
			if c.acc && (p == nil || p["tcp_ms"] == nil || p["icmp_ms"] == nil) {
				t.Errorf("%s %s: probe %v", name, c.target, p)
			}
			if pip, _ := p["ip"].(string); c.acc && strings.Contains(pip, ":") != c.v6 {
				t.Errorf("%s %s: probe went over the wrong family: %v", name, c.target, p)
			}
		}
	}
}
