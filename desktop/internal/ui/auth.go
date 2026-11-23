package ui

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/store"
)

// pageAuth: who may use the page.
//
//   - Listening on loopback only (Windows default) and no password set:
//     open, but only for Host: localhost/127.0.0.1/[::1] (DNS rebinding).
//   - Otherwise a password is required. Until it is set the page shows
//     a login form for the setup code from the data dir (the terminal
//     menu `sudo svyazest-agent` shows it): until an own password is set
//     the code is the password (1.0.7). Setting an own one drops the code.
//   - Sessions: random 32-byte cookie (HttpOnly, SameSite=Strict), stored
//     hashed in the data dir, 30 days sliding.
//   - Every POST needs X-Svyazest: 1 (no cross-site form can send it) and
//     a same-host Origin if one is sent; unknown Host names are refused.
//   - Password: PBKDF2-SHA256 (120k), login attempts limited per address.
//   - The CLI talks to the running agent with X-Local-Token (file in the
//     data dir, readable by root / Administrators only).
type pageAuth struct {
	st           *store.Store
	j            *store.Journal
	loopbackOnly bool
	localToken   string

	mu       sync.Mutex
	sessions map[string]time.Time // sha256(token) hex -> expiry
	fails    map[string]*failRec
	lastSave time.Time
	passSeen string // Settings.UIPassSet the sessions belong to
}

type failRec struct {
	n     int
	until time.Time
	first time.Time
}

const (
	cookieName  = "svyazest_session"
	sessionTTL  = 30 * 24 * time.Hour
	pbkdf2Iter  = 120000
	minPassword = 8
)

func newPageAuth(st *store.Store, j *store.Journal, loopbackOnly bool, localToken string) *pageAuth {
	a := &pageAuth{st: st, j: j, loopbackOnly: loopbackOnly, localToken: localToken, fails: map[string]*failRec{}}
	a.sessions = st.LoadSessions()
	a.passSeen = st.Settings().UIPassSet
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	return a
}

func (a *pageAuth) passwordSet() bool { return a.st.Settings().UIPassHash != "" }

// required: a password protects the page.
func (a *pageAuth) required() bool { return a.passwordSet() || !a.loopbackOnly }

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(h, "[]")
}

func isLoopbackHost(h string) bool {
	h = strings.ToLower(hostOnly(h))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func fromLoopback(r *http.Request) bool {
	ip := net.ParseIP(remoteIP(r))
	return ip != nil && ip.IsLoopback() && isLoopbackHost(r.Host)
}

// hostAllowed: IP literals, localhost, this machine's name; anything else
// is a DNS-rebinding attempt (or an extra name in SVYAZEST_UI_HOSTS).
func hostAllowed(h string) bool {
	host := strings.ToLower(hostOnly(h))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || isLoopbackHost(host) {
		return true
	}
	if hn, err := os.Hostname(); err == nil {
		hn = strings.ToLower(hn)
		short := strings.SplitN(hn, ".", 2)[0]
		for _, c := range []string{hn, short, short + ".local", short + ".lan", short + ".home", short + ".localdomain"} {
			if host == c {
				return true
			}
		}
	}
	for _, x := range strings.Split(os.Getenv("SVYAZEST_UI_HOSTS"), ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" && x == host {
			return true
		}
	}
	return false
}

// guard: headers + Host / CSRF checks for every request.
func (a *pageAuth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !hostAllowed(r.Host) {
			http.Error(w, "Неизвестный адрес страницы. Откройте её по IP-адресу компьютера или добавьте имя в SVYAZEST_UI_HOSTS.", http.StatusForbidden)
			return
		}
		if a.loopbackOnly && !a.passwordSet() && !isLoopbackHost(r.Host) {
			http.Error(w, "Откройте страницу по адресу http://127.0.0.1:8780/", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Svyazest") != "1" {
				writeErr(w, http.StatusForbidden, "Запрос отклонён")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				oh := o
				if i := strings.Index(oh, "://"); i >= 0 {
					oh = oh[i+3:]
				}
				if !strings.EqualFold(oh, r.Host) {
					writeErr(w, http.StatusForbidden, "Запрос с чужой страницы отклонён")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *pageAuth) sessionOK(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) < 32 {
		return false
	}
	k := hashToken(c.Value)
	set := a.st.Settings().UIPassSet
	a.mu.Lock()
	defer a.mu.Unlock()
	if set != a.passSeen {
		// password changed (maybe by `svyazest-agent ui-password`): the
		// sessions file was reset with it
		a.passSeen = set
		a.sessions = a.st.LoadSessions()
	}
	exp, ok := a.sessions[k]
	if !ok || time.Now().After(exp) {
		return false
	}
	// sliding expiry, saved at most every 10 min
	a.sessions[k] = time.Now().Add(sessionTTL)
	if time.Since(a.lastSave) > 10*time.Minute {
		a.lastSave = time.Now()
		a.st.SaveSessions(a.sessions)
	}
	return true
}

func (a *pageAuth) localOK(r *http.Request) bool {
	t := r.Header.Get("X-Local-Token")
	return a.localToken != "" && t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(a.localToken)) == 1
}

func (a *pageAuth) authed(r *http.Request) bool {
	if a.localOK(r) {
		return true
	}
	if !a.required() {
		return true
	}
	// 1.0.7: without an own password the session comes from the setup code.
	return a.sessionOK(r)
}

func (a *pageAuth) require(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Нужен вход на страницу", "auth": true})
			return
		}
		h(w, r)
	}
}

func (a *pageAuth) requireLocal(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.localOK(r) {
			writeErr(w, http.StatusUnauthorized, "local token required")
			return
		}
		h(w, r)
	}
}

func (a *pageAuth) handleStatus(w http.ResponseWriter, r *http.Request) {
	set := a.passwordSet()
	writeJSON(w, 200, map[string]any{
		"required":        a.required(),
		"password_set":    set,
		"authed":          a.authed(r),
		"need_setup_code": !set && !fromLoopback(r),
		"code_login":      !set && a.required(), // 1.0.7: the setup code is the password
		"loopback_only":   a.loopbackOnly,
	})
}

func (a *pageAuth) newSession(w http.ResponseWriter, r *http.Request) {
	tok := store.RandomHex(32)
	set := a.st.Settings().UIPassSet
	a.mu.Lock()
	a.passSeen = set
	a.sessions[hashToken(tok)] = time.Now().Add(sessionTTL)
	a.lastSave = time.Now()
	a.st.SaveSessions(a.sessions)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()), Secure: r.TLS != nil,
	})
}

// limited: too many wrong passwords / codes from this address.
func (a *pageAuth) limited(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[ip]
	if f == nil {
		return false, 0
	}
	if d := time.Until(f.until); d > 0 {
		return true, d
	}
	return false, 0
}

func (a *pageAuth) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[ip]
	if f == nil || time.Since(f.first) > 15*time.Minute {
		f = &failRec{first: time.Now()}
		a.fails[ip] = f
	}
	f.n++
	if f.n >= 5 {
		// 5 wrong in 15 min: 1 min, doubling up to 30 min
		d := time.Minute << uint(min(f.n-5, 5))
		f.until = time.Now().Add(d)
	}
	if len(a.fails) > 1000 {
		for k, v := range a.fails {
			if time.Since(v.first) > 15*time.Minute {
				delete(a.fails, k)
			}
		}
	}
}

func (a *pageAuth) ok(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

func HashPassword(pass string) (hash, salt string, iter int, err error) {
	sb := []byte(store.RandomHex(16))
	k, err := pbkdf2.Key(sha256.New, pass, sb, pbkdf2Iter, 32)
	if err != nil {
		return "", "", 0, err
	}
	return base64.StdEncoding.EncodeToString(k), string(sb), pbkdf2Iter, nil
}

func checkPassword(set store.Settings, pass string) bool {
	if set.UIPassHash == "" {
		return false
	}
	iter := set.UIPassIter
	if iter <= 0 {
		iter = pbkdf2Iter
	}
	k, err := pbkdf2.Key(sha256.New, pass, []byte(set.UIPassSalt), iter, 32)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(set.UIPassHash)
	return err == nil && subtle.ConstantTimeCompare(k, want) == 1
}

// SetPassword: also used by `svyazest-agent ui-password`.
func SetPassword(st *store.Store, pass string) error {
	h, s, it, err := HashPassword(pass)
	if err != nil {
		return err
	}
	_, err = st.UpdateSettings(func(x *store.Settings) {
		x.UIPassHash, x.UIPassSalt, x.UIPassIter = h, s, it
		x.UIPassSet = time.Now().UTC().Format(time.RFC3339)
	})
	if err == nil {
		st.DropSetupCode()
		st.SaveSessions(map[string]time.Time{}) // every old session ends
	}
	return err
}

func (a *pageAuth) handleSetup(w http.ResponseWriter, r *http.Request) {
	if a.passwordSet() {
		writeErr(w, 409, "Пароль уже задан")
		return
	}
	ip := remoteIP(r)
	if lim, d := a.limited(ip); lim {
		writeErr(w, 429, "Слишком много попыток. Подождите "+humanDur(d)+".")
		return
	}
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	if !fromLoopback(r) {
		if !a.codeOK(in.Code) {
			a.fail(ip)
			time.Sleep(700 * time.Millisecond)
			writeErr(w, 403, "Неверный код настройки. Его показывает команда «"+setupCodeCmd()+"» на этом компьютере.")
			return
		}
	}
	if len([]rune(in.Password)) < minPassword {
		writeErr(w, 400, "Пароль — не короче 8 символов")
		return
	}
	if err := SetPassword(a.st, in.Password); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	a.ok(ip)
	a.j.Info("пароль страницы агента задан (с адреса %s)", ip)
	a.newSession(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *pageAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if lim, d := a.limited(ip); lim {
		writeErr(w, 429, "Слишком много попыток. Подождите "+humanDur(d)+".")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	// 1.0.7: until an own password is set, the setup code is the password
	// (nothing to invent; the code is readable by root only).
	codeLogin := !a.passwordSet()
	good := false
	if codeLogin {
		good = a.codeOK(in.Password)
	} else {
		good = checkPassword(a.st.Settings(), in.Password)
	}
	if !good {
		a.fail(ip)
		time.Sleep(700 * time.Millisecond)
		a.j.Warn("неверный пароль страницы агента (с адреса %s)", ip)
		if codeLogin {
			writeErr(w, 403, "Неверный код. Его показывает меню агента на этом компьютере: «"+setupCodeCmd()+"».")
		} else {
			writeErr(w, 403, "Неверный пароль")
		}
		return
	}
	a.ok(ip)
	a.newSession(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *pageAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, hashToken(c.Value))
		a.st.SaveSessions(a.sessions)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *pageAuth) handleChange(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if lim, d := a.limited(ip); lim {
		writeErr(w, 429, "Слишком много попыток. Подождите "+humanDur(d)+".")
		return
	}
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if readJSON(r, &in) != nil {
		writeErr(w, 400, "Неверный запрос")
		return
	}
	if a.passwordSet() && !checkPassword(a.st.Settings(), in.Old) {
		a.fail(ip)
		time.Sleep(700 * time.Millisecond)
		writeErr(w, 403, "Текущий пароль неверный")
		return
	}
	if len([]rune(in.New)) < minPassword {
		writeErr(w, 400, "Пароль — не короче 8 символов")
		return
	}
	if err := SetPassword(a.st, in.New); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.mu.Lock()
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	a.j.Info("пароль страницы агента изменён (с адреса %s)", ip)
	a.newSession(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func humanDur(d time.Duration) string {
	if d < time.Minute {
		return "минуту"
	}
	m := int(d.Minutes() + 0.5)
	return itoa(m) + " мин"
}

func itoa(v int) string {
	if v <= 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// codeOK: the setup code, typed with or without the dash, any case.
func (a *pageAuth) codeOK(in string) bool {
	want := a.st.SetupCode(true)
	got := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(in), " ", ""))
	if len(got) == 8 && !strings.Contains(got, "-") {
		got = got[:4] + "-" + got[4:]
	}
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// setupCodeCmd: the command that prints the setup code (docker exec when
// the agent runs in the container).
func setupCodeCmd() string {
	if store.InDocker() {
		return "sudo docker exec -it svyazest-agent svyazest-agent"
	}
	return "sudo svyazest-agent"
}
