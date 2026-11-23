// Package ui: the agent's own web page (embedded, no CDN) on :8780 and its
// JSON API under /ui/api/*. Live updates over Server-Sent Events.
//
// Access: Windows listens on 127.0.0.1 only; Linux on 0.0.0.0 (headless
// Raspberry Pi) and then the page requires a password, set on first visit
// with a one-time setup code that only root can read (`svyazest-agent
// status`), or without the code from the machine itself. See auth.go.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/runner"
	"svyazest-agent/internal/store"
	"svyazest-agent/internal/ui/qr"
)

//go:embed static
var staticFS embed.FS

type Options struct {
	Runner       *runner.Runner
	Store        *store.Store
	Journal      *store.Journal
	API          *api.Client
	Login        *runner.Login
	Device       api.DeviceMeta
	Version      string
	Listen       string
	XrayPath     string
	XrayNextPath string
	LocalToken   string
	// Restart: «Перезапустить службу» (service manager restart, else the loop).
	Restart      func()
	SetAutostart func(bool) error
	ServiceState func() string
}

type Server struct {
	o            Options
	loopbackOnly bool
	auth         *pageAuth

	xrayOnce sync.Once
	xrayMain string
	xrayNext string

	profMu   sync.Mutex
	profBusy bool
}

func New(o Options) *Server {
	s := &Server{o: o}
	host, _, _ := net.SplitHostPort(o.Listen)
	ip := net.ParseIP(host)
	s.loopbackOnly = host == "localhost" || (ip != nil && ip.IsLoopback())
	s.auth = newPageAuth(o.Store, o.Journal, s.loopbackOnly, o.LocalToken)
	return s
}

func (s *Server) LoopbackOnly() bool { return s.loopbackOnly }

// ListenAndServe until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.o.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", files))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := fs.ReadFile(sub, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		b, _ := fs.ReadFile(sub, "icon.svg")
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(b)
	})

	// page auth (open)
	mux.HandleFunc("GET /ui/api/auth", s.auth.handleStatus)
	mux.HandleFunc("POST /ui/api/auth/setup", s.auth.handleSetup)
	mux.HandleFunc("POST /ui/api/auth/login", s.auth.handleLogin)
	mux.HandleFunc("POST /ui/api/auth/logout", s.auth.handleLogout)

	// everything else needs the session (or no password policy)
	a := s.auth.require
	mux.HandleFunc("POST /ui/api/auth/password", a(s.auth.handleChange))
	mux.HandleFunc("GET /ui/api/state", a(s.handleState))
	mux.HandleFunc("GET /ui/api/events", a(s.handleEvents))
	mux.HandleFunc("POST /ui/api/login/start", a(s.handleLoginStart))
	mux.HandleFunc("POST /ui/api/login/cancel", a(s.handleLoginCancel))
	mux.HandleFunc("POST /ui/api/login/code", a(s.handleLoginCode))
	mux.HandleFunc("GET /ui/api/login/qr.svg", a(s.handleQR))
	mux.HandleFunc("POST /ui/api/logout", a(s.handleLogout))
	mux.HandleFunc("GET /ui/api/history", a(s.handleHistory))
	mux.HandleFunc("GET /ui/api/modems", a(s.handleModems))
	mux.HandleFunc("POST /ui/api/modems/refresh", a(s.handleModemsRefresh))
	mux.HandleFunc("GET /ui/api/settings", a(s.handleSettings))
	mux.HandleFunc("POST /ui/api/settings", a(s.handleSettingsSave))
	mux.HandleFunc("POST /ui/api/restart", a(s.handleRestart))
	mux.HandleFunc("POST /ui/api/diag/decision", a(s.handleDiagDecision))
	mux.HandleFunc("POST /ui/api/diag/stop", a(s.handleDiagStop))
	mux.HandleFunc("GET /ui/api/journal", a(s.handleJournal))
	mux.HandleFunc("POST /ui/api/profile/refresh", a(s.handleProfileRefresh))

	// CLI <-> running agent (X-Local-Token only)
	mux.HandleFunc("GET /ui/api/local/status", s.auth.requireLocal(s.handleLocalStatus))
	mux.HandleFunc("POST /ui/api/local/logout", s.auth.requireLocal(s.handleLogout))

	return s.auth.guard(mux)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	return dec.Decode(v)
}

func (s *Server) xray() (string, string) {
	s.xrayOnce.Do(func() {
		s.xrayMain, s.xrayNext = checks.XrayVersions(checks.Env{XrayPath: s.o.XrayPath, XrayNextPath: s.o.XrayNextPath})
	})
	return s.xrayMain, s.xrayNext
}

// cachedProfile: never blocks the page on the network; a stale cache is
// refreshed in the background.
func (s *Server) cachedProfile() *api.Profile {
	p, at := s.o.Runner.CachedProfile()
	if p != nil && time.Since(at) < 60*time.Second {
		return p
	}
	s.profMu.Lock()
	busy := s.profBusy
	s.profBusy = true
	s.profMu.Unlock()
	if !busy {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = s.o.Runner.Profile(ctx, 30*time.Second)
			s.profMu.Lock()
			s.profBusy = false
			s.profMu.Unlock()
		}()
	}
	return p
}

// ---- state ----

type stateDTO struct {
	Status  runner.Status     `json:"status"`
	Login   runner.LoginState `json:"login"`
	Profile map[string]any    `json:"profile,omitempty"`
	About   map[string]any    `json:"about"`
	Now     time.Time         `json:"now"`
}

func (s *Server) state() stateDTO {
	st := s.o.Runner.Snapshot()
	var prof map[string]any
	if st.LoggedIn {
		if p := s.cachedProfile(); p != nil {
			prof = p.Raw
		}
	}
	main, next := s.xray()
	set := s.o.Store.Settings()
	about := map[string]any{
		"version":        s.o.Version,
		"platform":       s.o.Device.Platform,
		"os":             s.o.Device.OSVersion,
		"model":          s.o.Device.Model,
		"xray":           main,
		"xray_next":      next,
		"data_dir":       s.o.Store.Dir,
		"protection":     store.ProtectionLabel(),
		"listen":         s.o.Listen,
		"loopback_only":  s.loopbackOnly,
		"password_set":   set.UIPassHash != "",
		"autostart":      set.AutostartOn(),
		"modem_iface":    set.ModemIface,
		"modem_override": s.o.Runner.ModemOverride(),
		"bot_url":        "https://t.me/svyaz_estt_bot",
	}
	if s.o.ServiceState != nil {
		about["service"] = s.o.ServiceState()
	}
	return stateDTO{Status: st, Login: s.o.Login.State(), Profile: prof, About: about, Now: time.Now()}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.state())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.o.Runner.Subscribe()
	defer cancel()
	send := func() bool {
		b, _ := json.Marshal(s.state())
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			// coalesce bursts
			if d := time.Since(last); d < 300*time.Millisecond {
				time.Sleep(300*time.Millisecond - d)
			}
			last = time.Now()
			if !send() {
				return
			}
		case <-tick.C:
			if !send() {
				return
			}
		}
	}
}

// ---- sign-in ----

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.o.Store.Token() != "" {
		writeErr(w, 409, "Вход уже выполнен")
		return
	}
	st, err := s.o.Login.Start(r.Context(), s.o.Runner.Notify)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "login": st})
		return
	}
	s.o.Runner.Notify()
	writeJSON(w, 200, map[string]any{"login": st})
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.o.Login.Cancel()
	s.o.Runner.Notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	if err := s.o.Login.Pair(r.Context(), in.Code); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.o.Runner.Notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	st := s.o.Login.State()
	if st.Status != "waiting" || st.URL == "" {
		writeErr(w, 404, "Нет активной ссылки")
		return
	}
	c, err := qr.Encode(st.URL)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(c.SVG("#111317", "#ffffff")))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.o.Login.Cancel()
	s.o.Journal.Info("выход из аккаунта (по запросу %s)", ifs(strings.Contains(r.URL.Path, "/local/"), "из командной строки", "со страницы агента"))
	s.o.Runner.Logout("")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- history ----

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"local": s.o.Store.History()}
	if s.o.Store.Token() != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		jobs, err := s.o.API.JobHistory(ctx)
		cancel()
		if err != nil {
			out["error"] = "Не удалось загрузить историю с сервера"
		} else {
			if jobs == nil {
				jobs = []api.HistoryJob{}
			}
			out["jobs"] = jobs
		}
	}
	writeJSON(w, 200, out)
}

// ---- modems & settings ----

func (s *Server) modemsDTO() map[string]any {
	mods, ifaces, at, merr := s.o.Runner.Modems()
	type opt struct {
		Value string `json:"value"`
		Label string `json:"label"`
		Modem bool   `json:"modem"`
		Up    bool   `json:"up"`
	}
	var opts []opt
	seen := map[string]bool{}
	for _, m := range mods {
		seen[m.Interface] = true
		label := m.Interface + " — " + nz(m.Model, "модем")
		if m.Operator != "" {
			label += ", " + m.Operator
		}
		if !m.Connected {
			label += " (нет сети)"
		}
		opts = append(opts, opt{Value: m.Interface, Label: label, Modem: true, Up: m.Connected})
	}
	var rest []opt
	for _, i := range ifaces {
		if seen[i.Name] || i.Kind == "loopback" {
			continue
		}
		kind := map[string]string{"ethernet": "Ethernet", "wifi": "Wi-Fi", "modem": "модем", "virtual": "виртуальный"}[i.Kind]
		label := i.Name + ifs(kind != "", " — "+kind, "") + ifs(!i.Up, " (выключен)", "")
		rest = append(rest, opt{Value: i.Name, Label: label, Up: i.Up})
	}
	sort.SliceStable(rest, func(a, b int) bool { return rest[a].Value < rest[b].Value })
	opts = append(opts, rest...)
	res := map[string]any{"modems": mods, "options": opts, "selected": s.o.Store.Settings().ModemIface}
	if !at.IsZero() {
		res["at"] = at
	}
	if merr != "" {
		res["error"] = merr
	}
	return res
}

func (s *Server) handleModems(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.modemsDTO())
}

func (s *Server) handleModemsRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	s.o.Runner.RefreshModems(ctx)
	writeJSON(w, 200, s.modemsDTO())
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	set := s.o.Store.Settings()
	writeJSON(w, 200, map[string]any{"modem_iface": set.ModemIface, "autostart": set.AutostartOn(), "ui_listen": set.UIListen})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ModemIface *string `json:"modem_iface"`
		Autostart  *bool   `json:"autostart"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	if in.ModemIface != nil {
		v := strings.TrimSpace(*in.ModemIface)
		if len(v) > 64 || strings.ContainsAny(v, "/\\\x00") {
			writeErr(w, 400, "Неверное имя интерфейса")
			return
		}
		if _, err := s.o.Store.UpdateSettings(func(st *store.Settings) { st.ModemIface = v }); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.o.Journal.Info("модем в настройках: %s", nz(v, "автоматически"))
		s.o.Runner.Restart()
	}
	if in.Autostart != nil {
		on := *in.Autostart
		if s.o.SetAutostart != nil {
			if err := s.o.SetAutostart(on); err != nil {
				writeErr(w, 500, "Автозапуск не изменён: "+err.Error())
				return
			}
		}
		_, _ = s.o.Store.UpdateSettings(func(st *store.Settings) { st.Autostart = &on })
		s.o.Journal.Info("автозапуск службы: %s", ifs(on, "включён", "выключен"))
	}
	s.o.Runner.Notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		if s.o.Restart != nil {
			s.o.Restart()
		} else {
			s.o.Runner.Restart()
		}
	}()
}

// ---- diag ----

func (s *Server) handleDiagDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Accept bool `json:"accept"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	st, err := s.o.Runner.DiagDecide(r.Context(), in.Accept)
	if err != nil {
		writeErr(w, 502, "Не удалось отправить ответ: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": st})
}

func (s *Server) handleDiagStop(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Runner.DiagStop(r.Context()); err != nil {
		writeErr(w, 502, "Не удалось остановить: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"lines": s.o.Journal.Tail(400)})
}

func (s *Server) handleProfileRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, err := s.o.Runner.Profile(ctx, 0)
	if err != nil {
		writeErr(w, 502, "Профиль не загрузился")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLocalStatus(w http.ResponseWriter, r *http.Request) {
	st := s.state()
	writeJSON(w, 200, map[string]any{"status": st.Status, "about": st.About, "login": st.Login})
}

func nz(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ifs(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
