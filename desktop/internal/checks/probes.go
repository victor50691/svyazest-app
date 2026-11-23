package checks

// Port of NativePlugin.kt whitelistProbe / hostProbe / subnetProbe /
// dnsQueryA, with every socket out of the modem (env.Net).

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/netbind"
)

// fallbackDNS: Yandex DNS, a resolver of its own for the DNS check and one
// of the addresses a whitelist keeps reachable.
const fallbackDNS = "77.88.8.8"

var errNoFamily = errors.New("no address of the family")

// modemUp = waitForCellular(maxWait): the modem interface exists and is up.
func modemUp(ctx context.Context, env Env, maxWait time.Duration) (bool, string) {
	if env.Net == nil {
		return false, "no cellular network"
	}
	deadline := time.Now().Add(maxWait)
	for {
		up, why := modemState(env)
		if up {
			return true, ""
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, why
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func modemState(env Env) (bool, string) {
	want := env.Net.Iface()
	cur := want
	if list, err := netbind.List(); err == nil {
		found := false
		for _, i := range list {
			if i.Name == want.Name {
				cur, found = i, true
				break
			}
		}
		if !found {
			return false, "no cellular network"
		}
	}
	if !cur.Up {
		return false, "no cellular network"
	}
	if len(cur.Addrs) == 0 && len(want.Addrs) == 0 {
		return false, "modem has no IP address"
	}
	return true, ""
}

// hostProbeResult runs ICMP and TCP:443 in parallel to ip.
func icmpTCP(ctx context.Context, env Env, ip netip.Addr, timeout time.Duration) (icmp, tcp any) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); icmp = icmpMs(ctx, env, ip, timeout) }()
	go func() { defer wg.Done(); tcp = tcpConnectMs(ctx, env, ip, 443, timeout) }()
	wg.Wait()
	return
}

func whitelistProbeImpl(ctx context.Context, env Env, allowed, control []string) map[string]any {
	const timeout = 3000 * time.Millisecond
	if ok, _ := modemUp(ctx, env, 3*time.Second); !ok {
		return map[string]any{"verdict": "no_cellular", "allowed": []any{}, "control": []any{}}
	}
	probeHost := func(host string) map[string]any {
		var addr netip.Addr
		via := any(nil)
		if a, err := resolveV4(ctx, env, host, 5*time.Second); err == nil {
			addr, via = a, "system"
		} else if a, ok := dnsQueryA(ctx, env, fallbackDNS, host, timeout); ok {
			addr, via = a, "fallback"
		}
		if !addr.IsValid() {
			return map[string]any{"host": host, "ip": nil, "icmp_ms": nil, "tcp_ms": nil, "ok": false, "dns": nil, "error": "dns"}
		}
		icmp, tcp := icmpTCP(ctx, env, addr, timeout)
		return map[string]any{"host": host, "ip": addr.String(), "icmp_ms": icmp, "tcp_ms": tcp,
			"ok": icmp != nil || tcp != nil, "dns": via, "error": nil}
	}
	wait := timeout*3 + 3*time.Second
	run := func(hosts []string) []map[string]any {
		out := make([]map[string]any, len(hosts))
		chans := make([]chan map[string]any, len(hosts))
		for i, h := range hosts {
			chans[i] = make(chan map[string]any, 1)
			go func(c chan map[string]any, h string) { c <- probeHost(h) }(chans[i], h)
		}
		deadline := time.After(wait)
		for i := range hosts {
			select {
			case r := <-chans[i]:
				out[i] = r
			case <-deadline:
				out[i] = map[string]any{"ok": false, "error": "TimeoutException"}
			case <-ctx.Done():
				out[i] = map[string]any{"ok": false, "error": "CancellationException"}
			}
		}
		return out
	}
	var allowedRes, controlRes []map[string]any
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); allowedRes = run(allowed) }()
	go func() { defer wg.Done(); controlRes = run(control) }()
	wg.Wait()

	count := func(rs []map[string]any, f func(map[string]any) bool) int {
		n := 0
		for _, r := range rs {
			if f(r) {
				n++
			}
		}
		return n
	}
	isOK := func(r map[string]any) bool { return r["ok"] == true }
	allowedOK, controlOK := count(allowedRes, isOK), count(controlRes, isOK)
	all := append(append([]map[string]any{}, allowedRes...), controlRes...)
	dnsSystem := count(all, func(r map[string]any) bool { return r["dns"] == "system" })
	dnsFallback := count(all, func(r map[string]any) bool { return r["dns"] == "fallback" })
	var resolverReachable any
	if dnsSystem == 0 && dnsFallback == 0 {
		r := netip.MustParseAddr(fallbackDNS)
		resolverReachable = tcpConnectMs(ctx, env, r, 53, timeout) != nil || icmpMs(ctx, env, r, timeout) != nil
	}
	dnsVerdict := "down"
	switch {
	case dnsSystem > 0:
		dnsVerdict = "ok"
	case dnsFallback > 0 || resolverReachable == true:
		dnsVerdict = "system_down"
	}
	verdict := "partial"
	switch {
	case len(allowed) == 0 || len(control) == 0:
		verdict = "unknown"
	case dnsVerdict == "system_down":
		verdict = "no_dns"
	case allowedOK == 0:
		verdict = "no_internet"
	case controlOK == 0:
		verdict = "active"
	case controlOK == len(control):
		verdict = "inactive"
	}
	return map[string]any{
		"verdict": verdict,
		"allowed": toAnyList(allowedRes),
		"control": toAnyList(controlRes),
		"dns": map[string]any{
			"verdict": dnsVerdict, "system": dnsSystem, "fallback": dnsFallback, "total": len(all),
			"resolver": fallbackDNS, "resolver_reachable": resolverReachable,
			// Android's «Частный DNS»; the desktop resolver asks the modem's
			// own DNS servers directly, there is no such layer.
			"private_dns": false, "private_dns_server": nil,
		},
	}
}

func toAnyList(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// dnsQueryA: one A query straight to server:53 over UDP out of the modem,
// bypassing the resolver. First IPv4 answer.
func dnsQueryA(ctx context.Context, env Env, server, host string, timeout time.Duration) (netip.Addr, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pc, err := env.Net.ListenPacket(ctx, "udp4", "0.0.0.0:0")
	if err != nil {
		return netip.Addr{}, false
	}
	defer pc.Close()
	go func() { <-ctx.Done(); pc.SetDeadline(time.Now()) }()
	_ = pc.SetDeadline(time.Now().Add(timeout))
	id := uint16(time.Now().UnixNano() & 0xffff)
	q := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimRight(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return netip.Addr{}, false
		}
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	srv := &net.UDPAddr{IP: net.ParseIP(server), Port: 53}
	if _, err := pc.WriteTo(q, srv); err != nil {
		return netip.Addr{}, false
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return netip.Addr{}, false
		}
		if ua, ok := from.(*net.UDPAddr); ok && !ua.IP.Equal(srv.IP) {
			continue
		}
		return parseDNSA(buf[:n], id)
	}
}

func parseDNSA(buf []byte, id uint16) (netip.Addr, bool) {
	n := len(buf)
	u16 := func(i int) int { return int(binary.BigEndian.Uint16(buf[i:])) }
	if n < 12 || uint16(u16(0)) != id || buf[3]&0x0f != 0 {
		return netip.Addr{}, false
	}
	qd, an := u16(4), u16(6)
	p := 12
	skipName := func() {
		for p < n {
			l := int(buf[p])
			if l == 0 {
				p++
				return
			}
			if l&0xc0 == 0xc0 {
				p += 2
				return
			}
			p += l + 1
		}
	}
	for i := 0; i < qd; i++ {
		skipName()
		p += 4
	}
	for i := 0; i < an; i++ {
		skipName()
		if p+10 > n {
			return netip.Addr{}, false
		}
		typ, l := u16(p), u16(p+8)
		p += 10
		if typ == 1 && l == 4 && p+4 <= n {
			return netip.AddrFrom4([4]byte{buf[p], buf[p+1], buf[p+2], buf[p+3]}), true
		}
		p += l
	}
	return netip.Addr{}, false
}

// hostProbe = NativePlugin.hostProbe(host, 5000, family): family 6 (1.0.1,
// Android 1.17.2) = an IPv6 address / the domain's AAAA and ICMPv6 echo;
// anything else = IPv4 as before.
func hostProbe(ctx context.Context, env Env, host string, family int) map[string]any {
	const timeout = 5000 * time.Millisecond
	if ok, _ := modemUp(ctx, env, 3*time.Second); !ok {
		return map[string]any{"host": host, "ip": nil, "icmp_ms": nil, "tcp_ms": nil, "error": "no_cellular"}
	}
	var addr netip.Addr
	var err error
	if family == 6 {
		var list []netip.Addr
		list, err = lookupFamily(ctx, env, host, 6, 8*time.Second)
		if err == nil && len(list) == 0 {
			err = errNoFamily
		}
		if err == nil {
			addr = list[0]
		}
	} else {
		addr, err = resolveV4(ctx, env, host, 8*time.Second)
	}
	if err != nil {
		return map[string]any{"host": host, "ip": nil, "icmp_ms": nil, "tcp_ms": nil, "error": "dns"}
	}
	icmp, tcp := icmpTCP(ctx, env, addr, timeout)
	return map[string]any{"host": host, "ip": addr.String(), "icmp_ms": icmp, "tcp_ms": tcp, "error": nil}
}

// subnetProbe = NativePlugin.subnetProbe(cidr, 1200, 32): all 256 addresses,
// ICMP + TCP 443 (80 only when 443 is silent).
func subnetProbe(ctx context.Context, env Env, cidr string) map[string]any {
	const timeout = 1200 * time.Millisecond
	const concurrency = 32
	base := cidr
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		base = cidr[:i]
	}
	octets := strings.Split(base, ".")
	if len(octets) != 4 {
		return map[string]any{"cidr": cidr, "error": "bad_cidr"}
	}
	prefix := strings.Join(octets[:3], ".")
	if ok, _ := modemUp(ctx, env, 3*time.Second); !ok {
		return map[string]any{"cidr": cidr, "error": "no_cellular"}
	}
	type row struct {
		done bool
		m    map[string]any
	}
	rows := make([]row, 256)
	var mu sync.Mutex
	sweep, cancel := context.WithTimeout(ctx, timeout*12+20*time.Second)
	defer cancel()
	started := time.Now()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for last := range jobs {
				ipStr := prefix + "." + strconv.Itoa(last)
				addr, err := netip.ParseAddr(ipStr)
				if err != nil || !addr.Is4() {
					continue // InetAddress.getByName would throw: not counted
				}
				icmp := icmpMs(sweep, env, addr, timeout)
				tcp443 := tcpConnectMs(sweep, env, addr, 443, timeout)
				var tcp80 any
				if tcp443 == nil {
					tcp80 = tcpConnectMs(sweep, env, addr, 80, timeout)
				}
				if sweep.Err() != nil {
					continue // cut by the sweep deadline: invokeAll cancels it
				}
				mu.Lock()
				rows[last] = row{true, map[string]any{"ip": ipStr, "icmp_ms": icmp, "tcp443_ms": tcp443, "tcp80_ms": tcp80}}
				mu.Unlock()
			}
		}()
	}
feed:
	for i := 0; i < 256; i++ {
		select {
		case jobs <- i:
		case <-sweep.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started).Milliseconds()
	probed := 0
	hosts := []map[string]any{}
	for _, r := range rows {
		if !r.done {
			continue
		}
		probed++
		if r.m["icmp_ms"] != nil || r.m["tcp443_ms"] != nil || r.m["tcp80_ms"] != nil {
			hosts = append(hosts, r.m)
		}
	}
	aliveICMP, aliveTCP := 0, 0
	for _, h := range hosts {
		if h["icmp_ms"] != nil {
			aliveICMP++
		}
		if h["tcp443_ms"] != nil || h["tcp80_ms"] != nil {
			aliveTCP++
		}
	}
	shown := hosts
	if len(shown) > 40 {
		shown = shown[:40]
	}
	return map[string]any{
		"cidr": cidr, "probed": probed, "total": 256, "alive": len(hosts),
		"alive_icmp": aliveICMP, "alive_tcp": aliveTCP, "hosts": toAnyList(shown),
		"elapsed_ms": elapsed, "error": nil,
	}
}

// checkSubnet = subnet_checker.dart: the raw probe -> SubnetCheckResult.toJson.
func checkSubnet(ctx context.Context, env Env, cidr string) (untestable bool, out map[string]any) {
	raw := subnetProbe(ctx, env, cidr)
	out = map[string]any{
		"cidr": cidr, "probed": 0, "total": 256, "alive": 0, "alive_icmp": 0, "alive_tcp": 0,
		"hosts": []any{}, "elapsed_ms": nil, "verdict": "unknown", "error": nil,
		"untestable": false, "untestable_reason": nil, "works": false, "connected": false,
	}
	if e, ok := raw["error"].(string); ok && e != "" {
		untestable = e == "no_cellular" || e == "bad_cidr"
		reason := e
		if e == "no_cellular" {
			reason = "cellular_unavailable"
		}
		out["error"], out["untestable"], out["untestable_reason"] = e, untestable, reason
		return
	}
	for _, k := range []string{"probed", "total", "alive", "alive_icmp", "alive_tcp", "hosts", "elapsed_ms"} {
		out[k] = raw[k]
	}
	alive := raw["alive"].(int)
	verdict := "alive"
	switch {
	case alive == 0:
		verdict = "silent"
	case raw["alive_icmp"].(int) == 0 || raw["alive_tcp"].(int) == 0:
		verdict = "partial"
	}
	out["verdict"] = verdict
	out["works"] = alive > 0
	out["connected"] = alive > 0
	return
}
