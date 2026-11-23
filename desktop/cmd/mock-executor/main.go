// mock-executor: an in-memory stand-in for DPI//Checker's /executor/v1/*
// (admin-panel/routes/executor/index.js) to run the agent against without
// touching the real queue. Same signature check as the server's
// requireSignature (crypto.verify('sha256', data, spkiPem, derSig)).
//
//	mock-executor -listen 127.0.0.1:18080 [-confirm-after 3s] [-diag] [-gps]
//
// Admin: GET /mock/state, POST /mock/enqueue?type=domain|ip|subnet|vpn_key|vpn_next|domain4|domain6|ip6|domain6_noaaaa,
// POST /mock/flag?name=moderation|desktop_disabled|outdated|country_unknown|whitelist_required&v=...
package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type device struct {
	ID         int64
	Executor   int64
	Token      string
	Pub, Prev  string
	AttestedAt time.Time
	Challenge  string
	AppVersion string
	Platform   string
	Meta       map[string]any
	LastHB     map[string]any
	Verdict    string
	SigOK      int
	SigBad     int
	Unsigned   int
	IPv6OK     bool      // heartbeat "ipv6" (1.0.1)
	IPv6At     time.Time // when it was last sent
}

type job struct {
	ID       int64          `json:"id"`
	CheckID  int64          `json:"check_id"`
	Resource string         `json:"resource"`
	Type     string         `json:"resource_type"`
	Protocol *string        `json:"protocol"`
	Core     *string        `json:"needs_core"`
	IPFamily *int           `json:"ip_family"`
	Status   string         `json:"status"`
	Device   int64          `json:"-"`
	Expires  time.Time      `json:"-"`
	Result   map[string]any `json:"result,omitempty"`
	Success  string         `json:"outcome,omitempty"`
	Created  time.Time      `json:"created_at"`
	Done     *time.Time     `json:"completed_at"`
	Alive    int            `json:"alive_pings"`
}

type login struct {
	Meta    map[string]any
	Created time.Time
	Token   string
}

type mock struct {
	mu       sync.Mutex
	devices  map[string]*device
	logins   map[string]*login
	jobs     []*job
	nextID   int64
	flags    map[string]string
	diag     map[string]any
	gps      map[string]any
	diagLogs int
	logs     []string
	reports  []map[string]any
	hbCount  int
	confirm  time.Duration
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address")
	confirm := flag.Duration("confirm-after", 3*time.Second, "auto-confirm Telegram login after")
	withDiag := flag.Bool("diag", false, "create a pending diag request after 2 heartbeats")
	withGps := flag.Bool("gps", false, "create a pending GPS request")
	noJobs := flag.Bool("no-jobs", false, "start with an empty queue")
	ipv6Jobs := flag.Bool("ipv6-jobs", false, "queue only ip_family 4/6 jobs (domain4, domain6, ip6, domain6_noaaaa)")
	flag.Parse()
	m := &mock{devices: map[string]*device{}, logins: map[string]*login{}, nextID: 100,
		flags: map[string]string{"whitelist_required": "false"}, confirm: *confirm}
	if *withDiag {
		m.flags["diag"] = "1"
	}
	if *withGps {
		m.gps = map[string]any{"id": 5, "status": "pending", "reason": "проверка мока"}
	}
	if *ipv6Jobs {
		for _, t := range []string{"domain6", "domain4", "ip6", "domain6_noaaaa"} {
			m.enqueue(t)
		}
	} else if !*noJobs {
		for _, t := range []string{"domain", "ip", "subnet", "vpn_key", "vpn_ss", "vpn_next"} {
			m.enqueue(t)
		}
	}
	mux := http.NewServeMux()
	p := "/executor/v1"
	mux.HandleFunc("POST "+p+"/pair", m.pair)
	mux.HandleFunc("POST "+p+"/auth/start", m.authStart)
	mux.HandleFunc("GET "+p+"/auth/poll", m.authPoll)
	mux.HandleFunc("POST "+p+"/heartbeat", m.auth(m.heartbeat))
	mux.HandleFunc("POST "+p+"/attest/challenge", m.auth(m.challenge))
	mux.HandleFunc("POST "+p+"/attest/desktop", m.auth(m.attestDesktop))
	mux.HandleFunc("GET "+p+"/jobs/poll", m.auth(m.sig(m.poll)))
	mux.HandleFunc("POST "+p+"/jobs/{id}/alive", m.auth(m.sig(m.alive)))
	mux.HandleFunc("POST "+p+"/jobs/{id}/release", m.auth(m.sig(m.release)))
	mux.HandleFunc("POST "+p+"/jobs/{id}/result", m.auth(m.sig(m.result)))
	mux.HandleFunc("GET "+p+"/jobs/history", m.auth(m.history))
	mux.HandleFunc("GET "+p+"/ip", m.auth(func(w http.ResponseWriter, r *http.Request, d *device) {
		h, _, _ := net.SplitHostPort(r.RemoteAddr)
		js(w, 200, map[string]any{"ip": h})
	}))
	mux.HandleFunc("POST "+p+"/log", m.auth(m.logMsg))
	mux.HandleFunc("POST "+p+"/network-report", m.auth(m.netReport))
	mux.HandleFunc("GET "+p+"/profile", m.auth(m.profile))
	mux.HandleFunc("POST "+p+"/diag/{id}/decision", m.auth(m.diagDecision))
	mux.HandleFunc("POST "+p+"/diag/{id}/stop", m.auth(m.diagStop))
	mux.HandleFunc("POST "+p+"/diag/{id}/logs", m.auth(m.diagLogsH))
	mux.HandleFunc("POST "+p+"/gps/{id}/decision", m.auth(m.gpsAnswer))
	mux.HandleFunc("POST "+p+"/gps/{id}/result", m.auth(m.gpsAnswer))
	mux.HandleFunc("GET /mock/state", m.state)
	mux.HandleFunc("POST /mock/enqueue", func(w http.ResponseWriter, r *http.Request) {
		js(w, 200, m.enqueue(r.URL.Query().Get("type")))
	})
	mux.HandleFunc("POST /mock/flag", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.flags[r.URL.Query().Get("name")] = r.URL.Query().Get("v")
		m.mu.Unlock()
		js(w, 200, map[string]any{"ok": true})
	})
	log.Printf("mock executor API on http://%s%s", *listen, p)
	log.Fatal(http.ListenAndServe(*listen, logReq(mux)))
}

func logReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/mock/") && !strings.HasSuffix(r.URL.Path, "/alive") {
			log.Printf("%s %s ver=%q platform=%q route=%q sig=%v", r.Method, r.URL.Path, r.Header.Get("X-App-Version"),
				r.Header.Get("X-App-Platform"), r.Header.Get("X-Net-Route"), r.Header.Get("X-Sig") != "")
		}
		h.ServeHTTP(w, r)
	})
}

func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func body(r *http.Request) map[string]any {
	m := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

func (m *mock) enqueue(t string) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	j := &job{ID: m.nextID, CheckID: 9000 + m.nextID, Status: "pending", Created: time.Now()}
	switch t {
	case "ip":
		j.Type, j.Resource = "ip", "1.1.1.1"
	case "subnet":
		j.Type, j.Resource = "subnet", "192.0.2.0/24"
	case "vpn_key":
		j.Type, j.Protocol = "vpn_key", sp("vless")
		j.Resource = "vless://11111111-2222-3333-4444-555555555555@203.0.113.10:443?type=tcp&security=tls&sni=example.com#mock"
	case "vpn_ss":
		j.Type, j.Protocol = "vpn_key", sp("shadowsocks")
		j.Resource = "ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:mockpass")) + "@203.0.113.11:8388#mock-ss"
	case "vpn_next":
		j.Type, j.Protocol, j.Core = "vpn_key", sp("vless"), sp("next")
		j.Resource = "vless://11111111-2222-3333-4444-666666666666@203.0.113.12:443?type=xhttp&security=tls&sni=example.org&path=%2Fx#next"
	case "domain4":
		j.Type, j.Resource, j.IPFamily = "domain", "example.com", ip(4)
	case "domain6":
		j.Type, j.Resource, j.IPFamily = "domain", "ya.ru", ip(6)
	case "domain6_noaaaa":
		j.Type, j.Resource, j.IPFamily = "domain", "github.com", ip(6)
	case "ip6":
		j.Type, j.Resource, j.IPFamily = "ip", "2606:4700:4700::1111", ip(6)
	default:
		j.Type, j.Resource = "domain", "example.com"
	}
	m.jobs = append(m.jobs, j)
	return j
}

// ---- auth ----

type handler func(w http.ResponseWriter, r *http.Request, d *device)

func (m *mock) auth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		m.mu.Lock()
		d := m.devices[tok]
		if d != nil {
			if v := r.Header.Get("X-App-Version"); v != "" {
				d.AppVersion = v
			}
		}
		outdated := m.flags["outdated"] == "1"
		m.mu.Unlock()
		if d == nil {
			js(w, 401, map[string]any{"error": "Invalid or inactive device token."})
			return
		}
		if outdated && strings.Contains(r.URL.Path, "/jobs/poll") {
			js(w, 403, map[string]any{"error": "Версия больше не поддерживается.", "code": "app_outdated", "min_app_version": "1.1.0", "update_url": "https://github.com/victor50691/svyazest-app/releases/tag/desktop"})
			return
		}
		h(w, r, d)
	}
}

// verify = Node crypto.verify('sha256', data, spkiPem, derSig).
func verify(pemStr, data, sigB64 string) bool {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return false
	}
	ek, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	h := sha256.Sum256([]byte(data))
	return ecdsa.VerifyASN1(ek, h[:], sig)
}

func (m *mock) sigOK(r *http.Request, d *device) (bool, string) {
	if d.Pub == "" {
		if r.Header.Get("X-Sig") == "" {
			d.Unsigned++
		}
		return true, "no key bound"
	}
	ts, _ := strconv.ParseInt(r.Header.Get("X-Sig-Ts"), 10, 64)
	sig := r.Header.Get("X-Sig")
	if ts == 0 || sig == "" {
		return false, "no signature"
	}
	if skew := time.Now().UnixMilli() - ts; skew > 600000 || skew < -600000 {
		return false, fmt.Sprintf("time skew %d s", skew/1000)
	}
	data := fmt.Sprintf("%s %s %d", r.Method, r.URL.Path, ts)
	for _, k := range []string{d.Pub, d.Prev} {
		if k != "" && verify(k, data, sig) {
			return true, "ok"
		}
	}
	return false, "signature mismatch"
}

func (m *mock) sig(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, d *device) {
		m.mu.Lock()
		ok, why := m.sigOK(r, d)
		if ok && d.Pub != "" {
			d.SigOK++
		} else if !ok {
			d.SigBad++
		}
		m.mu.Unlock()
		if !ok {
			log.Printf("signature refused (device %d): %s", d.ID, why)
			js(w, 403, map[string]any{"error": "Запрос не подписан ключом устройства.", "code": "signature_required"})
			return
		}
		h(w, r, d)
	}
}

// ---- sign-in ----

func (m *mock) newDevice(meta map[string]any) map[string]any {
	m.nextID++
	d := &device{ID: m.nextID, Executor: 777000111, Token: randHex(32), Meta: meta}
	if p, ok := meta["platform"].(string); ok {
		d.Platform = p
	}
	// one device per account: retire the others
	for k, o := range m.devices {
		if o.Executor == d.Executor {
			delete(m.devices, k)
		}
	}
	m.devices[d.Token] = d
	log.Printf("device %d created: %v", d.ID, meta)
	return map[string]any{"device_token": d.Token, "device_id": d.ID, "executor_id": strconv.FormatInt(d.Executor, 10)}
}

func (m *mock) pair(w http.ResponseWriter, r *http.Request) {
	b := body(r)
	code, _ := b["code"].(string)
	if strings.ToUpper(strings.TrimSpace(code)) != "TEST1234" {
		js(w, 404, map[string]any{"error": "Invalid or expired code."})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	js(w, 200, m.newDevice(b))
}

func (m *mock) authStart(w http.ResponseWriter, r *http.Request) {
	b := body(r)
	code := randHex(24)
	m.mu.Lock()
	m.logins[code] = &login{Meta: b, Created: time.Now()}
	m.mu.Unlock()
	js(w, 200, map[string]any{"code": code, "url": "https://t.me/svyaz_estt_bot?start=app_" + code, "expires_in": 600})
}

func (m *mock) authPoll(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.logins[code]
	if l == nil {
		js(w, 404, map[string]any{"status": "expired"})
		return
	}
	if l.Token != "" {
		js(w, 200, map[string]any{"status": "expired"})
		return
	}
	if time.Since(l.Created) < m.confirm {
		js(w, 200, map[string]any{"status": "pending"})
		return
	}
	res := m.newDevice(l.Meta)
	l.Token = res["device_token"].(string)
	res["status"] = "ok"
	js(w, 200, res)
}

// ---- heartbeat & attestation ----

func (m *mock) heartbeat(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	d.LastHB = b
	if v, _ := b["whitelist_verdict"].(string); v != "" {
		d.Verdict = v
	}
	if v, ok := b["ipv6"].(bool); ok {
		if v != d.IPv6OK || d.IPv6At.IsZero() {
			log.Printf("device %d: ipv6=%v", d.ID, v)
		}
		d.IPv6OK, d.IPv6At = v, time.Now()
	}
	m.hbCount++
	if m.flags["diag"] == "1" && m.diag == nil && m.hbCount >= 2 {
		m.diag = map[string]any{"id": 7, "status": "pending", "reason": "проверка мока"}
	}
	var diag any
	if m.diag != nil && (m.diag["status"] == "pending" || m.diag["status"] == "accepted") {
		diag = m.diag
	}
	var gps any
	if m.gps != nil && m.gps["status"] == "pending" {
		gps = m.gps
	}
	js(w, 200, map[string]any{
		"ok": true, "whitelist_allowed": []string{"ya.ru", "vk.ru"}, "whitelist_control": []string{"google.com", "wikipedia.org"},
		"whitelist_interval": 120, "whitelist_required": m.flags["whitelist_required"] == "true",
		"diag": diag, "gps": gps, "attest_due": d.Pub != "" && time.Since(d.AttestedAt) > 23*time.Hour,
	})
}

func (m *mock) challenge(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	d.Challenge = base64.StdEncoding.EncodeToString([]byte(randHex(16)))
	ch := d.Challenge
	m.mu.Unlock()
	js(w, 200, map[string]any{"challenge": ch, "expires_in": 300})
}

func (m *mock) attestDesktop(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	pub, _ := b["pubkey"].(string)
	ch, _ := b["challenge"].(string)
	sig, _ := b["signature"].(string)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Pub != "" {
		if ok, why := m.sigOK(r, d); !ok {
			log.Printf("attest/desktop refused (bound key): %s", why)
			js(w, 403, map[string]any{"error": "Запрос не подписан ключом устройства.", "code": "signature_required"})
			return
		}
	}
	if ch == "" || ch != d.Challenge {
		js(w, 200, map[string]any{"status": "failed", "reason": "challenge_mismatch"})
		return
	}
	if !verify(pub, ch, sig) {
		js(w, 200, map[string]any{"status": "failed", "reason": "bad_signature"})
		return
	}
	if pub != d.Pub {
		d.Prev, d.Pub = d.Pub, pub
	}
	d.AttestedAt, d.Challenge = time.Now(), ""
	log.Printf("device %d key bound (desktop)", d.ID)
	js(w, 200, map[string]any{"status": "desktop"})
}

// ---- jobs ----

func (m *mock) poll(w http.ResponseWriter, r *http.Request, d *device) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		if f := m.flags["moderation"]; f != "" {
			m.mu.Unlock()
			time.Sleep(2 * time.Second)
			js(w, 200, map[string]any{"job": nil, "moderation": f})
			return
		}
		if m.flags["desktop_disabled"] == "1" {
			m.mu.Unlock()
			time.Sleep(2 * time.Second)
			js(w, 200, map[string]any{"job": nil, "desktop_disabled": true})
			return
		}
		if m.flags["country_unknown"] == "1" {
			m.mu.Unlock()
			time.Sleep(2 * time.Second)
			js(w, 200, map[string]any{"job": nil, "country_unknown": true, "need_cellular_ip": true})
			return
		}
		now := time.Now()
		// The server's ipv6Capable: desktop >= 1.0.1 with a fresh ipv6=true.
		canIPv6 := d.IPv6OK && time.Since(d.IPv6At) < 30*time.Minute && versionAtLeast(d.AppVersion, 1, 0, 1)
		var pick *job
		for _, j := range m.jobs {
			if (j.Status == "assigned" || j.Status == "running") && now.After(j.Expires) {
				log.Printf("job %d lease expired -> pending", j.ID)
				j.Status, j.Device = "pending", 0
			}
		}
		for _, j := range m.jobs {
			if j.Status == "pending" && (j.IPFamily == nil || *j.IPFamily != 6 || canIPv6) {
				pick = j
				break
			}
		}
		if pick != nil {
			pick.Status, pick.Device, pick.Expires = "assigned", d.ID, now.Add(15*time.Second)
			out := map[string]any{"id": pick.ID, "check_id": pick.CheckID, "resource": pick.Resource, "resource_type": pick.Type,
				"protocol": pick.Protocol, "needs_core": pick.Core, "ip_family": pick.IPFamily,
				"probe_sites": []string{"instagram.com", "telegram.org", "google.com"}, "download_url": "https://vin.proof.ovh.us/files/1Mb.dat",
				"whitelist_allowed": []string{"ya.ru", "vk.ru"}, "whitelist_control": []string{"google.com", "wikipedia.org"}}
			m.mu.Unlock()
			log.Printf("job %d (%s, ip_family %v) -> device %d", pick.ID, pick.Type, fam(pick.IPFamily), d.ID)
			js(w, 200, map[string]any{"job": out})
			return
		}
		m.mu.Unlock()
		if time.Now().After(deadline) {
			js(w, 200, map[string]any{"job": nil})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *mock) find(r *http.Request, d *device) *job {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	for _, j := range m.jobs {
		if j.ID == id && j.Device == d.ID {
			return j
		}
	}
	return nil
}

func (m *mock) alive(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(r, d)
	if j == nil || (j.Status != "assigned" && j.Status != "running") {
		js(w, 404, map[string]any{"error": "Job not found."})
		return
	}
	j.Status, j.Expires = "running", time.Now().Add(15*time.Second)
	j.Alive++
	js(w, 200, map[string]any{"ok": true})
}

func (m *mock) release(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(r, d)
	rel := j != nil && j.Status == "assigned"
	if rel {
		j.Status, j.Device = "pending", 0
	}
	js(w, 200, map[string]any{"ok": true, "released": rel})
}

func (m *mock) result(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(r, d)
	if j == nil || (j.Status != "assigned" && j.Status != "running") {
		js(w, 404, map[string]any{"error": "Job not found, not yours, or already finalized."})
		return
	}
	st, _ := b["status"].(string)
	if st != "completed" && st != "failed" {
		js(w, 400, map[string]any{"error": "status must be 'completed' or 'failed'."})
		return
	}
	res, _ := b["result"].(map[string]any)
	j.Status, j.Result, j.Success = st, res, st
	now := time.Now()
	j.Done = &now
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	log.Printf("job %d result: %s, %d alive pings, keys=%v", j.ID, st, j.Alive, keys)
	if res != nil && (j.IPFamily != nil || res["ip_family"] != nil) {
		pr, _ := res["probe"].(map[string]any)
		var pip any
		if pr != nil {
			pip = pr["ip"]
		}
		log.Printf("job %d ip_family=%v -> result ip_family=%v accessible=%v resolved_ip=%v block=%v untestable=%v probe_ip=%v",
			j.ID, fam(j.IPFamily), res["ip_family"], res["accessible"], res["resolved_ip"], res["block_reason"], res["untestable_reason"], pip)
	}
	js(w, 200, map[string]any{"ok": true})
}

func (m *mock) history(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for i := len(m.jobs) - 1; i >= 0; i-- {
		j := m.jobs[i]
		if j.Device != d.ID {
			continue
		}
		reward := "0.0000"
		if j.Status == "completed" {
			reward = "0.0040"
		}
		out = append(out, map[string]any{"id": j.ID, "resource_type": j.Type, "protocol": j.Protocol, "status": j.Status,
			"reward_amount": reward, "created_at": j.Created, "completed_at": j.Done})
	}
	js(w, 200, map[string]any{"jobs": out})
}

func (m *mock) logMsg(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	m.mu.Lock()
	m.logs = append(m.logs, fmt.Sprint(b["message"]))
	m.mu.Unlock()
	js(w, 200, map[string]any{"ok": true})
}

func (m *mock) netReport(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	b["ip"] = h
	b["at"] = time.Now().Format(time.RFC3339)
	b["device"] = d.ID
	m.mu.Lock()
	defer m.mu.Unlock()
	// The server keeps one report a minute per device.
	for i := len(m.reports) - 1; i >= 0; i-- {
		if m.reports[i]["device"] == d.ID {
			if t, err := time.Parse(time.RFC3339, m.reports[i]["at"].(string)); err == nil && time.Since(t) < time.Minute {
				js(w, 200, map[string]any{"ok": true, "throttled": true})
				return
			}
			break
		}
	}
	m.reports = append(m.reports, b)
	log.Printf("network report from device %d: via=%v ipv4=%v ipv6=%v ip_error=%v error=%v operator=%v country=%v vpn=%v",
		d.ID, b["via"], b["ipv4"], b["ipv6"], b["ip_error"], b["error"], b["operator_name"], b["network_country"], b["vpn_active"])
	js(w, 200, map[string]any{"ok": true, "ip": h, "ipv4": b["ipv4"], "ipv6": b["ipv6"]})
}

func fam(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// versionAtLeast: "1.0.1" (+build) >= a.b.c, missing parts = 0.
func versionAtLeast(v string, want ...int) bool {
	v = strings.SplitN(strings.TrimSpace(v), "+", 2)[0]
	parts := strings.Split(v, ".")
	for i, w := range want {
		n := 0
		if i < len(parts) {
			x, err := strconv.Atoi(parts[i])
			if err != nil {
				return false
			}
			n = x
		}
		if n != w {
			return n > w
		}
	}
	return true
}

func (m *mock) profile(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	done := 0
	for _, j := range m.jobs {
		if j.Device == d.ID && j.Status == "completed" {
			done++
		}
	}
	var diag any
	if m.diag != nil && (m.diag["status"] == "pending" || m.diag["status"] == "accepted") {
		diag = m.diag
	}
	mod := m.flags["moderation"]
	m.mu.Unlock()
	if mod == "" {
		mod = "approved"
	}
	earned := time.Now().Add(-48 * time.Hour)
	js(w, 200, map[string]any{
		"id": strconv.FormatInt(d.Executor, 10), "balance": fmt.Sprintf("%.4f", 1.2345+float64(done)*0.004),
		"total_earned": "3.1000", "total_jobs_completed": 120 + done, "moderation_status": mod, "moderation_reason": nil,
		"operator": "beeline", "operator_label": "Билайн", "region": "Новосибирская область", "federal_district_label": "Сибирский",
		"pending_application": nil, "traffic_bytes_total": 734003200, "country": "ru", "country_allowed": true,
		"allowed_countries": []string{"ru"}, "attestation_status": nil, "app_version": d.AppVersion, "app_outdated": false,
		"diag": diag, "gps": nil, "gps_supported": false,
		"available_balance": 1.0, "held_balance": 0.2345 + float64(done)*0.004, "hold_days": 3,
		"level": map[string]any{"level": 2, "points": 340, "level_min": 200, "next_min": 600, "share": 60, "next_share": 70,
			"streak_days": 4, "streak_today": true,
			"achievements": []map[string]any{
				{"code": "first_job", "emoji": "🎯", "title": "Первое задание", "desc": "Выполнено первое задание", "earned_at": earned},
				{"code": "week", "emoji": "📅", "title": "Неделя на связи", "desc": "7 дней подряд онлайн", "earned_at": nil},
				{"code": "wl_hero", "emoji": "🛡", "title": "Под белыми списками", "desc": "100 заданий при белых списках", "earned_at": nil},
			}},
	})
}

func (m *mock) diagDecision(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.diag == nil {
		js(w, 404, map[string]any{"error": "Not found."})
		return
	}
	if m.diag["status"] == "pending" {
		if b["accept"] == true {
			m.diag["status"] = "accepted"
		} else {
			m.diag["status"] = "declined"
		}
	}
	log.Printf("diag decision: %v", m.diag["status"])
	js(w, 200, map[string]any{"ok": true, "status": m.diag["status"]})
}

func (m *mock) diagStop(w http.ResponseWriter, r *http.Request, d *device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.diag != nil {
		m.diag["status"] = "stopped"
	}
	js(w, 200, map[string]any{"ok": true})
}

func (m *mock) diagLogsH(w http.ResponseWriter, r *http.Request, d *device) {
	var b struct {
		Lines []struct {
			T int64  `json:"t"`
			S string `json:"s"`
			M string `json:"m"`
		} `json:"lines"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.diag == nil || m.diag["status"] != "accepted" {
		js(w, 200, map[string]any{"ok": true, "active": false})
		return
	}
	m.diagLogs += len(b.Lines)
	// targets must never be in diag lines
	for _, l := range b.Lines {
		for _, j := range m.jobs {
			if strings.Contains(l.M, j.Resource) || strings.Contains(l.M, "203.0.113.") || strings.Contains(l.M, "example.com") {
				log.Printf("!!! TARGET LEAK in diag line: %q", l.M)
				break
			}
		}
	}
	js(w, 200, map[string]any{"ok": true, "active": true})
}

func (m *mock) gpsAnswer(w http.ResponseWriter, r *http.Request, d *device) {
	b := body(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gps != nil {
		m.gps["status"] = "answered"
		m.gps["answer"] = b
	}
	log.Printf("gps answer %s: %v", r.URL.Path, b)
	js(w, 200, map[string]any{"ok": true, "stored": true})
}

func (m *mock) state(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var devs []map[string]any
	for _, d := range m.devices {
		devs = append(devs, map[string]any{"id": d.ID, "platform": d.Platform, "app_version": d.AppVersion, "meta": d.Meta,
			"key_bound": d.Pub != "", "sig_ok": d.SigOK, "sig_bad": d.SigBad, "unsigned_before_bind": d.Unsigned,
			"last_heartbeat": d.LastHB, "verdict": d.Verdict, "ipv6_ok": d.IPv6OK, "ipv6_at": d.IPv6At})
	}
	js(w, 200, map[string]any{"devices": devs, "jobs": m.jobs, "diag": m.diag, "diag_lines": m.diagLogs, "gps": m.gps,
		"logs": m.logs, "network_reports": m.reports, "flags": m.flags})
}
