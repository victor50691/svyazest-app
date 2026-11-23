package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/modem"
	"svyazest-agent/internal/netbind"
	"svyazest-agent/internal/store"
)

// A single check may take a while (a /24 sweep, a slow key); past this it
// is abandoned and reported as untestable.
const jobTimeout = 15 * time.Minute

func TypeLabel(t string) string {
	switch t {
	case "vpn_key":
		return "VPN-ключ"
	case "ip", "domain":
		return "Домен/IP"
	case "subnet":
		return "Подсеть /24"
	}
	return t
}

func describe(t string) string {
	switch t {
	case "vpn_key":
		return "Проверка VPN-ключа"
	case "subnet":
		return "Проверка подсети"
	}
	return "Проверка домена/IP"
}

var jsonAddrRe = regexp.MustCompile(`"(?:address|server|host|serverName|sni)"\s*:\s*"([^"]+)"`)
var uriHostRe = regexp.MustCompile(`@\[?([A-Za-z0-9._:-]+)\]?:\d+`)

// hideResource = Android _hideResource: the resource, its host, any address
// in a JSON config and long query values never reach a log line.
func (r *Runner) hideResource(res string) {
	r.j.Secret(res)
	for _, m := range jsonAddrRe.FindAllStringSubmatch(res, -1) {
		r.j.Secret(m[1])
	}
	for _, m := range uriHostRe.FindAllStringSubmatch(res, -1) {
		r.j.Secret(m[1])
	}
	raw := res
	if !strings.Contains(raw, "://") {
		raw = "x://" + raw
	}
	if u, err := url.Parse(raw); err == nil {
		r.j.Secret(u.Hostname())
		if u.User != nil {
			r.j.Secret(u.User.Username())
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				if len(v) >= 6 {
					r.j.Secret(v)
				}
			}
		}
		if u.Fragment != "" {
			r.j.Secret(u.Fragment)
		}
	}
	// "1.2.3.0/24" -> also the bare network
	if i := strings.IndexByte(res, '/'); i > 0 && !strings.Contains(res, "://") {
		r.j.Secret(res[:i])
	}
}

// alive pings POST /jobs/:id/alive every 5 s (and when the check calls
// Env.Alive), so a vanished agent gives the job back within ~15 s.
type alive struct {
	mu   sync.Mutex
	last time.Time
	n    int
	stop chan struct{}
	once sync.Once
}

func (r *Runner) startAlive(ctx context.Context, id int64) *alive {
	a := &alive{stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		a.ping(ctx, r, id)
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.stop:
				return
			case <-t.C:
				a.ping(ctx, r, id)
			}
		}
	}()
	return a
}

func (a *alive) ping(ctx context.Context, r *Runner, id int64) {
	a.mu.Lock()
	if time.Since(a.last) < 4*time.Second {
		a.mu.Unlock()
		return
	}
	a.last = time.Now()
	a.n++
	n := a.n
	a.mu.Unlock()
	select {
	case <-a.stop:
		return
	default:
	}
	t0 := time.Now()
	err := r.api.JobAlive(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			r.j.Debug("[задание %d] продление аренды #%d НЕ УДАЛОСЬ за %d мс: %s — через 15 с без продления задание отдадут другому",
				id, n, time.Since(t0).Milliseconds(), shortErr(err))
		}
		return
	}
	if n == 1 || n%6 == 0 {
		r.j.Debug("[задание %d] продление аренды #%d ок за %d мс", id, n, time.Since(t0).Milliseconds())
	}
}

func (a *alive) Stop() { a.once.Do(func() { close(a.stop) }) }

func (r *Runner) releaseJob(id int64, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	released, err := r.api.ReleaseJob(ctx, id)
	if err != nil {
		r.j.Warn("задание %d не возвращено в очередь (%s): %s", id, why, shortErr(err))
		return
	}
	r.j.Info("задание %d возвращено в очередь (%s)%s", id, why, ifs(!released, " — уже было в работе, сервер отдаст его другому через 15 с"))
}

// runJob returns the pause before the next poll (10 s after a job the
// modem could not carry, like Android after cellular_unavailable).
func (r *Runner) runJob(ctx context.Context, job checks.Job, m modem.Info, b netbind.Binder) time.Duration {
	r.hideResource(job.Resource)
	started := time.Now()
	r.j.Info("задание %d получено: %s%s", job.ID, TypeLabel(job.ResourceType), ifs(job.Protocol != "", ", "+job.Protocol))
	_ = r.trace(ctx, fmt.Sprintf("job %d received (%s)", job.ID, job.ResourceType))
	r.report("running", "Работает", describe(job.ResourceType))
	r.update(func(s *Status) {
		s.Job = &JobView{ID: job.ID, Type: job.ResourceType, Label: TypeLabel(job.ResourceType), Since: started}
	})

	// The lease pings must outlive the cycle context only as long as the
	// job is ours: a restart / stop cancels both.
	al := r.startAlive(ctx, job.ID)
	defer al.Stop()

	var success bool
	var result map[string]any

	// Modem vanished between poll and run: refuse honestly (the customer is
	// refunded), like Android's cellular_unavailable.
	cur, ok := r.currentModem()
	if !ok || !cur.Connected || cur.Interface != m.Interface {
		success = false
		result = map[string]any{
			"untestable": true, "untestable_reason": "cellular_unavailable",
			"error": "modem disconnected", "vpn_active": false, "bound_cellular": false,
		}
	} else {
		jctx, cancel := context.WithTimeout(ctx, jobTimeout)
		env := r.env(b)
		env.Alive = func() { al.ping(ctx, r, job.ID) }
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.j.Warn("[задание %d] сбой проверки: %v", job.ID, p)
					success, result = false, map[string]any{"untestable": true, "untestable_reason": "internal_error", "error": fmt.Sprint(p)}
				}
			}()
			success, result = checks.Run(jctx, env, job)
		}()
		timedOut := jctx.Err() == context.DeadlineExceeded
		cancel()
		if ctx.Err() != nil {
			// Stopped / restarted mid-check: give the job back.
			al.Stop()
			r.releaseJob(job.ID, "служба остановлена во время проверки")
			r.store(job, "released", started, "прервано остановкой службы")
			return 0
		}
		if result == nil {
			success = false
			result = map[string]any{"untestable": true, "untestable_reason": "internal_error", "error": "no result"}
		}
		if timedOut {
			success = false
			result["untestable"] = true
			if _, ok := result["untestable_reason"]; !ok {
				result["untestable_reason"] = "timeout"
			}
		}
	}
	if w, ok := result["whitelist"].(map[string]any); ok {
		if v, _ := w["verdict"].(string); v != "" && v != "unknown" {
			r.setVerdict(v, w)
		}
	}
	if result["untestable_reason"] == "ipv6_unavailable" {
		// The modem lost IPv6 since the last measurement: the server
		// learns it with the next heartbeat (Android 1.17.2 does the same).
		off := false
		r.ipv6Ok, r.ipv6At = &off, time.Now()
		r.j.Info("[задание %d] задание IPv6, но у модема сейчас нет IPv6 — проверить нельзя, IPv6-задания отключены до следующего замера", job.ID)
	}
	for _, k := range []string{"resolved_ip", "final_host", "payload_url"} {
		if v, ok := result[k].(string); ok {
			r.j.Secret(v)
		}
	}
	r.j.Debug("[задание %d] проверка за %d мс: успех=%v; %s", job.ID, time.Since(started).Milliseconds(), success,
		summary(job.ResourceType, result))

	status := r.submit(ctx, al, job, success, result)
	al.Stop()
	note := ""
	switch status {
	case "completed", "failed":
		total := 0
		r.st.UpdateState(func(st *store.State) { st.JobsTotal++; total = st.JobsTotal })
		r.update(func(s *Status) {
			s.JobsDone++
			s.JobsTotal = total
			now := time.Now()
			s.LastJobAt = &now
		})
		r.mu.Lock()
		r.profAt = time.Time{} // balance changed: refresh /profile
		r.mu.Unlock()
		r.j.Info("задание %d выполнено за %.1f с: %s", job.ID, time.Since(started).Seconds(),
			ifs(success, "проверка проведена")+ifs(!success, "проверить не удалось"))
	case "lost":
		note = "сервер уже закрыл задание"
	default:
		note = "результат не доставлен"
	}
	r.store(job, status, started, note)
	r.consecutiveErr = 0
	if result["untestable_reason"] == "cellular_unavailable" {
		return 10 * time.Second
	}
	return 0
}

func (r *Runner) store(job checks.Job, status string, started time.Time, note string) {
	r.st.AddHistory(store.HistoryEntry{
		JobID: job.ID, Type: job.ResourceType, Status: status, At: time.Now(),
		DurationMs: time.Since(started).Milliseconds(), Note: note,
	})
	r.Notify()
}

// submit: POST /jobs/:id/result with retries on network / 5xx errors
// (3, 6, 12, 24 s); the lease pings keep running meanwhile. Returns
// completed | failed | lost (404: no longer ours) | undelivered.
func (r *Runner) submit(ctx context.Context, al *alive, job checks.Job, success bool, result map[string]any) string {
	delay := 3 * time.Second
	sigRetried := false
	size := 0
	if b, err := json.Marshal(result); err == nil {
		size = len(b)
	}
	for attempt := 1; ; attempt++ {
		t0 := time.Now()
		sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err := r.api.SubmitResult(sctx, job.ID, success, result)
		cancel()
		if err == nil {
			r.j.Debug("[задание %d] результат отправлен за %d мс через «%s» (%d Б JSON)", job.ID, time.Since(t0).Milliseconds(), r.api.Route(), size)
			if success {
				return "completed"
			}
			return "failed"
		}
		if e, ok := api.AsError(err); ok {
			switch {
			case e.Status == 404:
				r.j.Warn("задание %d: сервер уже закрыл его (срок аренды истёк или отдано другому)", job.ID)
				return "lost"
			case e.Code == "signature_required" && !sigRetried:
				sigRetried = true
				r.reattest(ctx, "сервер не принял подпись результата", false)
				continue
			case e.Status == 401 || e.DeviceBanned() || e.AccountBanned() || e.Status == 400 || e.Status == 403:
				r.j.Warn("задание %d: результат отклонён: %s", job.ID, e.Error())
				r.apiError(ctx, err, "отправка результата")
				return "undelivered"
			}
		}
		if attempt >= 5 || ctx.Err() != nil {
			r.j.Warn("задание %d: результат не доставлен после %d попыток: %s", job.ID, attempt, shortErr(err))
			return "undelivered"
		}
		r.j.Debug("[задание %d] отправка результата #%d не удалась: %s — повтор через %d с", job.ID, attempt, shortErr(err), int(delay.Seconds()))
		r.report("running", "Работает", "Отправка результата… (повтор)")
		sleepCtx(ctx, delay, nil)
		delay *= 2
	}
}

// trace: breadcrumb on the server's device row (POST /log), best effort.
func (r *Runner) trace(ctx context.Context, msg string) error {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return r.api.Log(tctx, r.j.Mask(msg))
}

// summary = Android _summary: verdict fields only, never the resource.
func summary(t string, res map[string]any) string {
	v := func(k string) string {
		if x, ok := res[k]; ok && x != nil {
			return fmt.Sprintf("%s=%v ", k, x)
		}
		return ""
	}
	switch t {
	case "vpn_key":
		ok, n := 0, 0
		if sites, isArr := res["sites"].([]any); isArr {
			n = len(sites)
			for _, s := range sites {
				if m, isMap := s.(map[string]any); isMap && m["ok"] == true {
					ok++
				}
			}
		}
		return v("works") + v("connected") + v("latency_ms") + fmt.Sprintf("сайты %d/%d ", ok, n) +
			v("sockets_bound") + v("sockets_unbound") + v("untestable_reason") + v("error")
	case "subnet":
		return v("alive") + v("probed") + v("verdict") + v("untestable_reason") + v("error")
	}
	return v("ip_family") + v("accessible") + v("tls_ok") + v("http_code") + v("time_ms") + v("tcp_latency_ms") + v("bytes") +
		v("block_reason") + v("untestable_reason") + v("error")
}

// jobsTotal: the all-time counter; the first time — from the local history.
func (r *Runner) jobsTotal() int {
	if st := r.st.State(); st.JobsTotalSeeded {
		return st.JobsTotal
	}
	n := 0
	for _, h := range r.st.History() {
		if h.Status == "completed" || h.Status == "failed" {
			n++
		}
	}
	total := 0
	r.st.UpdateState(func(st *store.State) {
		if !st.JobsTotalSeeded {
			st.JobsTotal, st.JobsTotalSeeded = st.JobsTotal+n, true
		}
		total = st.JobsTotal
	})
	return total
}
