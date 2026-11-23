// Package runner: the service loop, a port of the Android app's
// executor_task_handler.dart -- heartbeat, the «белые списки» probe on the
// server's schedule, long poll, alive pings, checks.Run over the modem,
// result with retries, and every stop/pause state the server can put us in.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/modem"
	"svyazest-agent/internal/netbind"
	"svyazest-agent/internal/store"
)

type Config struct {
	Store         *store.Store
	Journal       *store.Journal
	API           *api.Client
	Ctl           netbind.Control
	Device        api.DeviceMeta
	XrayPath      string
	XrayNextPath  string
	APIBase       string
	ModemOverride string // --modem-iface: use this interface as "the modem"
	Version       string
}

// Status: what the UI shows (and `svyazest-agent status` prints).
type Status struct {
	Code     string    `json:"code"`
	Headline string    `json:"headline"`
	Text     string    `json:"text"`
	Tone     string    `json:"tone"` // ok | warn | bad | idle
	Since    time.Time `json:"since"`

	LoggedIn   bool   `json:"logged_in"`
	ExecutorID string `json:"executor_id,omitempty"`
	Notice     string `json:"notice,omitempty"` // why we were signed out

	Modem     *ModemView    `json:"modem,omitempty"`
	Route     string        `json:"route"`
	Server    ServerView    `json:"server"` // 1.0.9: «Связь с сервером» сейчас
	Whitelist WhitelistView `json:"whitelist"`
	Job       *JobView      `json:"job,omitempty"`
	JobsDone  int           `json:"jobs_done"`
	JobsTotal int           `json:"jobs_total"` // за всё время на этом компьютере (1.0.5)
	LastJobAt *time.Time    `json:"last_job_at,omitempty"`

	Diag   *api.Request `json:"diag,omitempty"`
	DiagOn bool         `json:"diag_on"`
	Gps    *GpsView     `json:"gps,omitempty"`
	Attest string       `json:"attest,omitempty"`
	MinApp string       `json:"min_app_version,omitempty"`
	// 1.0.6: from the profile, for the terminal menu (the page reads the
	// profile itself): partner application and «update required».
	Moderation       string    `json:"moderation,omitempty"` // pending | approved | rejected
	ModerationReason string    `json:"moderation_reason,omitempty"`
	AppOutdated      bool      `json:"app_outdated,omitempty"`
	UpdateURL        string    `json:"update_url,omitempty"`
	Country          string    `json:"country,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	Started          time.Time `json:"started"`
}

type ModemView struct {
	Interface string `json:"interface"`
	Model     string `json:"model"`
	Operator  string `json:"operator"`
	MCCMNC    string `json:"mccmnc"`
	Country   string `json:"country"`
	Tech      string `json:"tech"`
	TechLabel string `json:"tech_label"`
	Signal    int    `json:"signal"`
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
	Manual    bool   `json:"manual"`
	// 1.0.6: Operator / Country came from the mobile network's IP (server).
	OperatorByIP bool `json:"operator_by_ip,omitempty"`
	CountryByIP  bool `json:"country_by_ip,omitempty"`
}

// ServerView: the last exchange with the server (api.Client.Link).
type ServerView struct {
	OK    bool       `json:"ok"`
	At    *time.Time `json:"at,omitempty"`
	Error string     `json:"error,omitempty"`
}

type WhitelistView struct {
	Verdict  string     `json:"verdict,omitempty"`
	Label    string     `json:"label,omitempty"`
	At       *time.Time `json:"at,omitempty"`
	Required bool       `json:"required"`
	DNS      string     `json:"dns,omitempty"`
}

type JobView struct {
	ID    int64     `json:"id"`
	Type  string    `json:"type"`
	Label string    `json:"label"`
	Since time.Time `json:"since"`
}

type GpsView struct {
	ID     int64     `json:"id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type Runner struct {
	cfg Config
	j   *store.Journal
	st  *store.Store
	api *api.Client

	mu      sync.RWMutex
	status  Status
	subs    map[chan struct{}]struct{}
	profile *api.Profile
	profAt  time.Time

	modems   modemState
	diag     *diagLog
	wake     chan struct{}
	restart  chan struct{}
	cycleMu  sync.Mutex
	cancelCy context.CancelFunc

	// loop state (owned by the loop goroutine)
	binder          netbind.Binder
	binderIface     string
	consecutiveErr  int
	offlineReported time.Time
	wlProbedAt      time.Time
	pendingVerdict  string
	lastVerdict     string
	reattestAt      time.Time
	pausedUntil     time.Time
	pausedCode      string
	pausedCountry   string
	netReportedKey  atomic.Value // string: "iface|ipv4|ipv6" the last network report went out with
	netReportAt     time.Time    // last network report attempt
	ipv6Ok          *bool        // heartbeat "ipv6": nil = not measured yet
	ipv6At          time.Time    // last IPv6 / echo measurement
	ipv6Iface       string
	attestNeeded    atomic.Bool
	resetPause      atomic.Bool
	diagChanged     map[string]string
	cellIP          map[string]any // last checks.CellularIP over the modem
	cellIPAt        time.Time
	cellIPIface     string
	wantCellularIP  bool
	pollHold        [3]string // code, headline, text from the last empty poll
	lastHB          *api.HeartbeatReply
}

func New(cfg Config) *Runner {
	r := &Runner{
		cfg:         cfg,
		j:           cfg.Journal,
		st:          cfg.Store,
		api:         cfg.API,
		subs:        map[chan struct{}]struct{}{},
		wake:        make(chan struct{}, 1),
		restart:     make(chan struct{}, 1),
		diagChanged: map[string]string{},
	}
	r.diag = newDiagLog(r)
	r.attestNeeded.Store(true)
	r.netReportedKey.Store("")
	now := time.Now()
	r.status = Status{Code: "starting", Headline: "Запуск", Text: "Запуск службы…", Tone: "idle", Since: now, Started: now,
		JobsTotal: r.jobsTotal()}
	st := r.st.State()
	if st.LastVerdict != "" && time.Since(st.LastVerdictAt) < 15*time.Minute {
		r.lastVerdict = st.LastVerdict
		t := st.LastVerdictAt
		r.status.Whitelist = WhitelistView{Verdict: st.LastVerdict, Label: VerdictLabel(st.LastVerdict), At: &t, Required: true}
	}
	r.status.Attest = st.AttestStatus
	return r
}

// ---- status & subscribers ----

func (r *Runner) Snapshot() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.status
	s.Route = r.api.Route()
	if ok, at, why := r.api.Link(); !at.IsZero() {
		s.Server = ServerView{OK: ok, At: &at, Error: why}
	}
	if d, ok := r.st.Device(); ok {
		s.LoggedIn, s.ExecutorID = true, d.ExecutorID
	}
	s.DiagOn = r.diag.activeID() > 0
	return s
}

// Subscribe: a channel that gets a tick whenever the status changes.
func (r *Runner) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

func (r *Runner) notifyLocked() {
	for ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (r *Runner) Notify() {
	r.mu.Lock()
	r.notifyLocked()
	r.mu.Unlock()
}

func (r *Runner) update(fn func(s *Status)) {
	r.mu.Lock()
	fn(&r.status)
	r.notifyLocked()
	r.mu.Unlock()
}

var tones = map[string]string{
	"online": "ok", "running": "ok",
	"no_whitelist": "warn", "moderation": "warn", "desktop_disabled": "warn", "country_unknown": "warn", "app_outdated": "warn", "attestation": "warn", "no_dns": "warn",
	"starting": "idle", "not_logged_in": "idle", "stopped": "idle",
}

// report = Android _report(): status code + headline + text.
func (r *Runner) report(code, headline, text string) {
	r.diagOnce("status", "статус: "+code+" — "+text)
	r.update(func(s *Status) {
		if s.Code != code {
			s.Since = time.Now()
		}
		s.Code, s.Headline, s.Text = code, headline, text
		if t, ok := tones[code]; ok {
			s.Tone = t
		} else {
			s.Tone = "bad"
		}
		if code != "running" {
			s.Job = nil
		}
	})
}

// diagOnce: a repeated state goes to the journal once, not every cycle.
func (r *Runner) diagOnce(kind, line string) {
	r.mu.Lock()
	same := r.diagChanged[kind] == line
	r.diagChanged[kind] = line
	r.mu.Unlock()
	if !same {
		r.j.Info("%s", line)
	}
}

// ---- control from the UI / CLI ----

// Wake: a login happened (or settings changed) -- don't wait out a pause.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Restart: abort the current cycle (and a running check) and start over.
func (r *Runner) Restart() {
	r.j.Info("перезапуск цикла службы")
	r.resetPause.Store(true)
	r.cycleMu.Lock()
	if r.cancelCy != nil {
		r.cancelCy()
	}
	r.cycleMu.Unlock()
}

// Logout: forget the token (the server has no logout endpoint; the next
// sign-in retires this device row anyway).
func (r *Runner) Logout(note string) {
	_ = r.st.ClearDevice()
	r.diag.setActive(0)
	r.st.UpdateState(func(s *store.State) { s.SignedOutNote = note; s.AttestStatus = "" })
	r.attestNeeded.Store(true)
	r.mu.Lock()
	r.profile = nil
	r.status.Moderation, r.status.ModerationReason, r.status.AppOutdated = "", "", false
	r.status.Notice = note
	r.status.Attest = ""
	r.mu.Unlock()
	r.Restart()
}

// ModemOverride: the --modem-iface value (wins over the settings).
func (r *Runner) ModemOverride() string { return r.cfg.ModemOverride }

// CachedProfile: the last /profile and when it was fetched (no network).
func (r *Runner) CachedProfile() (*api.Profile, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.profile, r.profAt
}

// Profile: cached /profile for the UI (refreshed by the loop and on demand).
func (r *Runner) Profile(ctx context.Context, maxAge time.Duration) (*api.Profile, error) {
	r.mu.RLock()
	p, at := r.profile, r.profAt
	r.mu.RUnlock()
	if p != nil && time.Since(at) < maxAge {
		return p, nil
	}
	if r.st.Token() == "" {
		return nil, api.ErrNotPaired
	}
	np, err := r.api.Profile(ctx)
	if err != nil {
		if p != nil {
			return p, err
		}
		return nil, err
	}
	r.mu.Lock()
	r.profile, r.profAt = &np, time.Now()
	r.status.MinApp, r.status.UpdateURL = np.MinAppVersion, np.UpdateURL
	r.status.Moderation, r.status.ModerationReason = np.ModerationStatus, np.ModerationReason
	r.status.AppOutdated = np.AppOutdated
	r.notifyLocked()
	r.mu.Unlock()
	return &np, nil
}

// ---- the loop ----

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	st := r.st.State()
	if !st.CleanStop && !st.RunningSince.IsZero() {
		r.j.Warn("прошлый запуск службы оборвался без остановки (сбой или выключение питания); последний признак жизни: %s",
			st.LastAliveAt.Local().Format("02.01.2006 15:04:05"))
	}
	r.st.UpdateState(func(s *store.State) { s.RunningSince = time.Now(); s.CleanStop = false })
	r.j.Info("служба запущена: версия %s, %s, %s", r.cfg.Version, r.cfg.Device.OSVersion, r.cfg.Device.Model)
	if st.DiagActiveID > 0 {
		r.diag.setActive(st.DiagActiveID)
	}
	go r.modemWatch(ctx)
	go r.diag.run(ctx)
	go r.profileLoop(ctx)
	r.api.OnRoute = func(route string) {
		r.j.Info("связь с сервером: %s", route)
		r.Notify()
	}
	for ctx.Err() == nil {
		cctx, cancel := context.WithCancel(ctx)
		r.cycleMu.Lock()
		r.cancelCy = cancel
		r.cycleMu.Unlock()
		r.loop(cctx)
		cancel()
	}
	r.shutdown()
}

func (r *Runner) shutdown() {
	r.j.Info("служба останавливается")
	if r.st.Token() != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = r.api.Heartbeat(ctx, api.Heartbeat{MobileDataEnabled: false, IsOnline: false, HWID: r.cfg.Device.HWID})
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r.diag.flush(ctx)
	cancel()
	r.st.UpdateState(func(s *store.State) { s.CleanStop = true })
	r.report("stopped", "Остановлено", "Служба остановлена")
}

func sleepCtx(ctx context.Context, d time.Duration, wake <-chan struct{}) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-wake:
	}
}

func (r *Runner) loop(ctx context.Context) {
	for ctx.Err() == nil {
		delay := r.cycle(ctx)
		r.st.UpdateState(func(s *store.State) { s.LastAliveAt = time.Now() })
		sleepCtx(ctx, delay, r.wake)
	}
}

// backoff: 3, 6, 12, 24, 48, 60 s.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := 3 * time.Second
	for i := 1; i < n && d < 60*time.Second; i++ {
		d *= 2
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// cycle: one pass of the Android while-loop; returns the pause before the next.
func (r *Runner) cycle(ctx context.Context) (delay time.Duration) {
	defer func() {
		if p := recover(); p != nil {
			r.consecutiveErr++
			r.j.Warn("сбой цикла: %v", p)
			r.report("error", "Сбой", fmt.Sprint(p))
			delay = backoff(r.consecutiveErr)
		}
	}()

	dev, ok := r.st.Device()
	if !ok {
		r.attestNeeded.Store(true)
		note := r.st.State().SignedOutNote
		r.update(func(s *Status) { s.Notice = note })
		r.report("not_logged_in", "Нет входа", "Войдите через Telegram на странице агента или в меню «sudo svyazest-agent»")
		return 3 * time.Second
	}
	_ = dev
	r.update(func(s *Status) { s.Notice = "" })

	// Paused by the server (outdated version, country): wait it out, check
	// /profile now and then instead of hammering heartbeat/poll.
	if r.resetPause.Swap(false) {
		r.pausedUntil, r.pausedCode = time.Time{}, ""
	}
	if !r.pausedUntil.IsZero() {
		if r.pauseOver(ctx) {
			r.pausedUntil, r.pausedCode = time.Time{}, ""
			r.j.Info("пауза снята, служба продолжает работу")
		} else {
			return 30 * time.Second
		}
	}

	// Key binding (desktop attestation) once per run after sign-in.
	if r.attestNeeded.Swap(false) {
		r.reattest(ctx, "запуск службы", true)
	}

	r.modems.mu.Lock()
	never := r.modems.at.IsZero()
	r.modems.mu.Unlock()
	if never {
		r.detectModems(ctx) // first cycle: don't judge before the first detection
	}
	m, have := r.currentModem()
	if !have || !m.Connected {
		return r.noModem(ctx, m, have)
	}
	r.offlineReported = time.Time{}
	binder, err := r.binderFor(m.Interface)
	if err != nil {
		r.report("modem_down", "Модем без сети", "Интерфейс модема "+m.Interface+" недоступен: "+shortErr(err))
		return r.noModem(ctx, m, false)
	}
	r.cfg.Ctl.SetModem(m.Interface)
	r.measureNet(ctx, m, binder)
	if ctx.Err() != nil {
		return 0
	}

	hb, err := r.api.Heartbeat(ctx, api.Heartbeat{
		MobileDataEnabled: true, IsOnline: true, HWID: r.cfg.Device.HWID,
		OperatorName: m.Operator, Country: m.Country, SimCountry: m.Country,
		WhitelistVerdict: r.pendingVerdict, Platform: r.cfg.Device.Platform,
		Modem:      modemReport(m),
		CellularIP: r.cellularIPForHB(ctx, m, binder),
		IPv6:       r.ipv6Ok,
	})
	if err != nil {
		return r.apiError(ctx, err, "heartbeat")
	}
	r.lastHB = &hb
	if r.pendingVerdict != "" {
		r.lastVerdict, r.pendingVerdict = r.pendingVerdict, ""
	}
	if m.Country == "" || m.Operator == "" {
		r.setNetInfo(m.Interface, ifs(m.Operator == "", hb.NetOperator), ifs(m.Country == "", hb.NetCountry))
	}
	r.update(func(s *Status) {
		s.Whitelist.Required = hb.WhitelistRequired
		s.Country = nz(m.Country, hb.NetCountry)
		s.Notice = ""
	})
	r.diag.sync(ctx, hb.Diag)
	r.gpsSync(ctx, hb.Gps)
	if hb.AttestDue {
		r.reattest(ctx, "сервер просит ежедневную перепроверку", false)
	}
	r.diagOnce("heartbeat", fmt.Sprintf("heartbeat ок: БС обязательны=%v, замер БС=%v, вердикт=%s; модем %s (оператор %s, страна %s)",
		hb.WhitelistRequired, hb.CanProbe(), nz(r.lastVerdict, "—"), m.Interface, nz(m.Operator, "—"), nz(m.Country, "—")))

	// «Белые списки» on the server's schedule.
	if hb.CanProbe() && (r.wlProbedAt.IsZero() || time.Since(r.wlProbedAt) >= hb.WhitelistInterval) {
		r.wlProbedAt = time.Now()
		r.report("online", "Работает", "Замер белых списков…")
		t0 := time.Now()
		wl := checks.WhitelistProbe(ctx, r.env(binder), hb.WhitelistAllowed, hb.WhitelistControl)
		if ctx.Err() != nil {
			return 0
		}
		r.j.Debug("замер белых списков за %d мс: %s", time.Since(t0).Milliseconds(), wlSummary(wl))
		if v, _ := wl["verdict"].(string); v != "" && v != "unknown" {
			r.setVerdict(v, wl)
		}
	}

	switch {
	case r.lastVerdict == "no_dns":
		r.report("no_dns", "Нет DNS", "DNS мобильной сети не отвечает — заданий нет")
	case hb.WhitelistRequired && r.lastVerdict != "" && r.lastVerdict != "active":
		r.report("no_whitelist", "Нет БС", "Белые списки у оператора не включены — заданий нет")
	case r.pollHold[0] != "":
		// what the last poll said (moderation, computers off, ...) stays
		// until a poll says otherwise -- no flapping to «Работает»
		r.report(r.pollHold[0], r.pollHold[1], r.pollHold[2])
	default:
		r.report("online", "Работает", "Ожидание заданий…")
	}

	t0 := time.Now()
	pr, err := r.api.PollJob(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		return r.apiError(ctx, err, "опрос заданий")
	}
	hold := func(code, head, text string) {
		r.pollHold = [3]string{code, head, text}
		r.report(code, head, text)
	}
	r.pollHold = [3]string{}
	if pr.Job == nil {
		r.consecutiveErr = 0
		r.update(func(s *Status) { s.LastError = "" })
		switch {
		case pr.Moderation != "" && pr.Moderation != "approved":
			if pr.Moderation == "rejected" {
				hold("moderation", "Заявка отклонена", "Подайте заявку заново в Telegram-боте «Связь Есть?»")
			} else {
				hold("moderation", "На модерации", "Приём заданий — после одобрения заявки")
			}
		case pr.DesktopDisabled:
			hold("desktop_disabled", "Компьютеры отключены", "Компьютеры временно отключены от заданий — агент ждёт")
		case pr.CountryUnknown || pr.NeedCellularIP:
			hold("country_unknown", "Определяем страну", "Определяем страну мобильной сети…")
			r.wantCellularIP = true
		case pr.AttestationRequired:
			hold("attestation", "Проверка ключа", "Сервер просит заново привязать ключ устройства")
			r.reattest(ctx, "сервер ждёт привязки ключа", false)
		case pr.WhitelistRequired && r.lastVerdict == "active":
			// Our verdict did not reach the server yet: next heartbeat carries it.
		}
		r.diagOnce("poll", "опрос: заданий нет")
		r.j.Debug("опрос заданий за %d мс: пусто", time.Since(t0).Milliseconds())
		return 0
	}
	job := *pr.Job
	if ctx.Err() != nil {
		r.releaseJob(job.ID, "служба останавливается")
		return 0
	}
	return r.runJob(ctx, job, m, binder)
}

func (r *Runner) currentCode() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status.Code
}

func (r *Runner) setVerdict(v string, wl map[string]any) {
	r.pendingVerdict, r.lastVerdict = v, v
	now := time.Now()
	dns := ""
	if d, ok := wl["dns"].(map[string]any); ok {
		dns, _ = d["verdict"].(string)
	}
	r.st.UpdateState(func(s *store.State) { s.LastVerdict, s.LastVerdictAt = v, now })
	r.update(func(s *Status) {
		s.Whitelist.Verdict, s.Whitelist.Label, s.Whitelist.At, s.Whitelist.DNS = v, VerdictLabel(v), &now, dns
	})
	r.diagOnce("verdict", "белые списки: "+VerdictLabel(v))
}

func VerdictLabel(v string) string {
	switch v {
	case "active":
		return "Включены"
	case "partial":
		return "Частично"
	case "inactive":
		return "Не включены"
	case "no_internet":
		return "Нет интернета"
	case "no_dns":
		return "Не работает DNS"
	}
	return v
}

func wlSummary(w map[string]any) string {
	if w == nil {
		return "не замерялись"
	}
	side := func(k string) string {
		arr, _ := w[k].([]any)
		var parts []string
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				okv := "нет"
				if m["ok"] == true {
					okv = "ок"
				}
				parts = append(parts, fmt.Sprintf("%v:%s", m["host"], okv))
			}
		}
		return strings.Join(parts, ",")
	}
	dns, _ := w["dns"].(map[string]any)
	return fmt.Sprintf("вердикт=%v; из БС: %s; контрольные: %s; DNS %v", w["verdict"], side("allowed"), side("control"), dns["verdict"])
}

// noModem: no data link -- tell the server once, then every 30 s.
func (r *Runner) noModem(ctx context.Context, m modem.Info, have bool) time.Duration {
	// The link is down: IPv6 is measured afresh when it is back.
	r.ipv6At, r.ipv6Ok = time.Time{}, nil
	if r.offlineReported.IsZero() || time.Since(r.offlineReported) > 30*time.Second {
		r.offlineReported = time.Now()
		hb := api.Heartbeat{MobileDataEnabled: false, IsOnline: true, HWID: r.cfg.Device.HWID, Platform: r.cfg.Device.Platform}
		if have {
			hb.OperatorName = m.Operator
			hb.Modem = modemReport(m)
		}
		if _, err := r.api.Heartbeat(ctx, hb); err != nil {
			r.offlineReported = time.Time{}
			if e, ok := api.AsError(err); ok {
				if d := r.apiError(ctx, e, "heartbeat"); d > 5*time.Second {
					return d
				}
			}
		}
	}
	code := r.currentCode()
	if code != "modem_down" || !have {
		if have {
			r.report("modem_down", "Модем без сети", "Модем найден ("+nz(m.Model, m.Interface)+"), но мобильный интернет не подключён")
		} else {
			r.report("no_modem", "Нет модема", "Подключите USB-модем или телефон в режиме USB-модема")
		}
	}
	return 5 * time.Second
}

// apiError = the Android ApiException / catch branches.
func (r *Runner) apiError(ctx context.Context, err error, where string) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	e, ok := api.AsError(err)
	if !ok {
		r.consecutiveErr++
		d := backoff(r.consecutiveErr)
		msg := shortErr(err)
		r.update(func(s *Status) { s.LastError = msg })
		text := fmt.Sprintf("Сервер недоступен, повтор через %d с", int(d.Seconds()))
		if r.lastVerdict == "active" {
			text = "Белые списки: нет связи с сервером — нужен Ethernet или Wi-Fi, отдельный от модема"
		}
		r.report("error", "Нет связи", text)
		if r.consecutiveErr <= 3 || r.consecutiveErr%10 == 0 {
			r.j.Warn("%s: ошибка сети #%d: %s", where, r.consecutiveErr, msg)
		}
		return d
	}
	r.j.Debug("ошибка API (%s): HTTP %d code=%s %s", where, e.Status, e.Code, e.Message)
	switch {
	case e.Code == "signature_required":
		r.reattest(ctx, "сервер не принял подпись запроса", false)
		return 3 * time.Second
	case e.Code == "attestation_failed":
		r.report("attestation", "Ключ не принят", "Проверка подлинности не пройдена: "+e.Message)
		r.reattest(ctx, "проверка подлинности не пройдена", false)
		return 60 * time.Second
	case e.CountryBlocked():
		m, _ := r.currentModem()
		_, ipCC := r.netInfo(m.Interface)
		cc := strings.ToUpper(nz(m.Country, nz(ipCC, "—")))
		r.pause("country_blocked", 10*time.Minute, m.Country)
		r.report("country_blocked", "Страна не поддерживается", "Мобильная сеть этой страны ("+cc+") не поддерживается")
		r.j.Warn("сервер: мобильная сеть страны %s не поддерживается — задания не назначаются", cc)
		return 30 * time.Second
	case e.AppOutdated():
		minv, _ := e.Body["min_app_version"].(string)
		upd, _ := e.Body["update_url"].(string)
		r.update(func(s *Status) { s.MinApp, s.UpdateURL, s.AppOutdated = minv, upd, true })
		r.pause("app_outdated", 30*time.Minute, "")
		r.report("app_outdated", "Обновитесь", "Эта версия агента больше не поддерживается"+ifs(minv != "", " (нужна "+minv+" или новее)"))
		r.j.Warn("сервер: версия %s больше не поддерживается (минимум %s) — обновите агент", r.cfg.Version, minv)
		return 30 * time.Second
	case e.Unauthorized():
		r.j.Warn("сеанс завершён сервером (401): вход выполнен на другом устройстве или устройство отключено")
		r.Logout("Сеанс завершён: в аккаунт вошли с другого устройства или устройство отключено. Войдите снова.")
		return 0
	case e.DeviceBanned():
		r.j.Warn("устройство заблокировано администратором")
		r.Logout("Это устройство заблокировано администратором «Связь Есть?».")
		return 0
	case e.AccountBanned():
		r.j.Warn("аккаунт заблокирован")
		r.Logout("Ваш аккаунт заблокирован. Подробности — в боте «Связь Есть?».")
		return 0
	}
	r.consecutiveErr++
	d := backoff(r.consecutiveErr)
	r.update(func(s *Status) { s.LastError = e.Error() })
	r.report("error", "Сбой", fmt.Sprintf("Ошибка сервера (%d), повтор через %d с", e.Status, int(d.Seconds())))
	if r.consecutiveErr <= 3 || r.consecutiveErr%10 == 0 {
		r.j.Warn("%s: %s", where, e.Error())
	}
	return d
}

func (r *Runner) pause(code string, d time.Duration, country string) {
	r.pausedCode, r.pausedUntil, r.pausedCountry = code, time.Now().Add(d), country
	// Like Android stopping its service: the server should stop counting
	// this device as available right away.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = r.api.Heartbeat(ctx, api.Heartbeat{MobileDataEnabled: false, IsOnline: false, HWID: r.cfg.Device.HWID})
}

// pauseOver: the reason for the pause is gone (or its time is up).
func (r *Runner) pauseOver(ctx context.Context) bool {
	switch r.pausedCode {
	case "country_blocked":
		if m, ok := r.currentModem(); ok && m.Country != "" && m.Country != r.pausedCountry {
			return true // another SIM / modem
		}
		// 1.0.7: the admin allowed the country (Настройки → страны) — go on
		// within about a minute instead of waiting the whole pause out.
		if p, err := r.Profile(ctx, time.Minute); err == nil && p.CountryAllowed != nil && *p.CountryAllowed {
			r.j.Info("сервер: страна %s теперь разрешена", strings.ToUpper(nz(p.Country, "—")))
			return true
		}
	case "app_outdated":
		if p, err := r.Profile(ctx, 5*time.Minute); err == nil && !p.AppOutdated {
			return true
		}
	}
	return time.Now().After(r.pausedUntil)
}

// reattest: bind the device key (POST /attest/desktop), not more often
// than every 10 minutes unless forced at start.
func (r *Runner) reattest(ctx context.Context, why string, start bool) {
	if !start && !r.reattestAt.IsZero() && time.Since(r.reattestAt) < 10*time.Minute {
		return
	}
	r.reattestAt = time.Now()
	actx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	status, err := r.api.AttestDesktop(actx)
	if err != nil {
		if e, ok := api.AsError(err); ok && e.Status == 403 && e.Code == "signature_required" {
			// The server holds another key for this device: ours was lost
			// (data dir restored/recreated). Only a new sign-in helps.
			r.j.Warn("ключ устройства не совпадает с привязанным на сервере — нужен повторный вход")
			r.Logout("Ключ устройства не совпадает с привязанным на сервере (каталог данных пересоздан?). Войдите снова.")
			return
		}
		if e, ok := api.AsError(err); ok && (e.Status == 404 || e.Status == 405) {
			status = "unsupported"
			r.j.Debug("привязка ключа (%s): сервер её пока не умеет (HTTP %d)", why, e.Status)
		} else {
			r.j.Warn("привязка ключа устройства (%s) не удалась: %s", why, shortErr(err))
			return
		}
	} else {
		r.j.Info("привязка ключа устройства (%s): %s", why, status)
	}
	r.st.UpdateState(func(s *store.State) { s.AttestStatus, s.AttestAt = status, time.Now() })
	r.update(func(s *Status) { s.Attest = status })
}

// gpsSync: a computer has no GPS -- answer the request once with the reason.
func (r *Runner) gpsSync(ctx context.Context, g *api.Request) {
	if g == nil {
		return
	}
	id := int64(g.ID)
	st := r.st.State()
	for _, x := range st.GpsAnswered {
		if x == id {
			return
		}
	}
	const reason = "На компьютере проверка местоположения недоступна: нет GPS"
	err := r.api.GpsResult(ctx, id, map[string]any{"error": reason, "source": "desktop"})
	if err != nil {
		if e, ok := api.AsError(err); !ok || e.Status >= 500 {
			r.j.Warn("ответ на проверку местоположения #%d не отправлен: %s", id, shortErr(err))
			return
		}
		_, _ = r.api.GpsDecision(ctx, id, false)
	}
	r.st.UpdateState(func(s *store.State) {
		s.GpsAnswered = append(s.GpsAnswered, id)
		if len(s.GpsAnswered) > 50 {
			s.GpsAnswered = s.GpsAnswered[len(s.GpsAnswered)-50:]
		}
	})
	r.j.Info("поддержка запросила проверку местоположения (#%d)%s — на компьютере её нет, отправлен отказ",
		id, ifs(g.Reason != "", ", причина: «"+g.Reason+"»"))
	r.update(func(s *Status) { s.Gps = &GpsView{ID: id, Reason: g.Reason, At: time.Now()} })
}

func (r *Runner) binderFor(iface string) (netbind.Binder, error) {
	if r.binder != nil && r.binderIface == iface {
		return r.binder, nil
	}
	b, err := netbind.ForInterface(iface)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, errors.New("netbind: нет привязки")
	}
	r.binder, r.binderIface = b, iface
	r.ipv6At = time.Time{} // new modem: measure IPv6 and report the network at once
	r.wlProbedAt = time.Time{}
	r.j.Info("проверки идут через интерфейс %s", iface)
	return b, nil
}

func (r *Runner) env(b netbind.Binder) checks.Env {
	return checks.Env{
		Net:          b,
		XrayPath:     r.cfg.XrayPath,
		XrayNextPath: r.cfg.XrayNextPath,
		APIBase:      r.cfg.APIBase,
		DeviceToken:  r.st.Token(),
		Log:          r.j.DebugLine,
		Secret:       r.j.Secret,
	}
}

func (r *Runner) profileLoop(ctx context.Context) {
	// Every minute, and within seconds of a new sign-in (1.0.6: the menu
	// shows «на модерации / отклонена» right after the login).
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	var lastTok string
	var lastAt time.Time
	for {
		if tok := r.st.Token(); tok != "" && (tok != lastTok || time.Since(lastAt) >= 60*time.Second) {
			maxAge := 30 * time.Second
			if tok != lastTok {
				maxAge = 0
			}
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			p, err := r.Profile(pctx, maxAge)
			cancel()
			lastTok, lastAt = tok, time.Now() // failed too: next try in a minute
			if err == nil && p != nil {
				r.diag.sync(ctx, p.Diag)
			}
		} else if tok == "" {
			lastTok = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func modemReport(m modem.Info) *api.ModemReport {
	return &api.ModemReport{
		Interface: m.Interface, Operator: m.Operator, MCCMNC: m.MCCMNC, Country: m.Country, Tech: m.Tech,
		Model: m.Model, Source: m.Source, SignalPct: m.SignalPct, Connected: m.Connected,
	}
}

// cellularIPForHB: the operator's public IP over the modem, for the
// server to tell the country when the modem gives no MCC (or when the
// server asked for it: need_cellular_ip). Re-measured every 5 minutes.
func (r *Runner) cellularIPForHB(ctx context.Context, m modem.Info, b netbind.Binder) map[string]any {
	if m.Country != "" && !r.wantCellularIP {
		return nil
	}
	fresh := r.cellIP != nil && r.cellIPIface == m.Interface && time.Since(r.cellIPAt) < 5*time.Minute
	if !fresh || r.wantCellularIP {
		r.wantCellularIP = false
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res := checks.CellularIP(cctx, r.env(b))
		cancel()
		r.cellIPAt, r.cellIPIface = time.Now(), m.Interface
		if res != nil {
			if ip, _ := res["ip"].(string); ip != "" {
				r.cellIP = map[string]any{"ip": ip, "via": res["via"]}
				r.j.Debug("IP мобильной сети узнан через %v", res["via"])
			} else {
				r.cellIP = nil
				r.j.Debug("IP мобильной сети не узнан: %v", res["error"])
			}
		}
	}
	return r.cellIP
}

// ---- small helpers ----

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func nz(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ifs(c bool, s string) string {
	if c {
		return s
	}
	return ""
}

var _ = http.MethodGet
