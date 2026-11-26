// svyazest-agent: «Связь Есть?» for computers (Linux incl. Raspberry Pi and
// Docker, Windows). Takes DPI//Checker jobs and runs them through the
// partner's mobile modem; talks to the server over another interface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/modem"
	"svyazest-agent/internal/netbind"
	"svyazest-agent/internal/runner"
	"svyazest-agent/internal/store"
	"svyazest-agent/internal/svc"
	"svyazest-agent/internal/ui"
	"svyazest-agent/internal/ui/qr"
)

// version: -ldflags "-X main.version=1.0.2"
var version = "1.0.10"

const usage = `Связь Есть? — агент для компьютера (версия %s)

Использование:
  svyazest-agent                     меню: состояние, вход, служба, журнал, обновление
  svyazest-agent run                 запустить службу (в терминале, под systemd / OpenRC / runit / SCM)
  svyazest-agent login               войти: ссылка Telegram + QR-код, или код из бота
  svyazest-agent status              состояние, адрес страницы, код настройки (пароль страницы)
  svyazest-agent logout              выйти из аккаунта
  svyazest-agent ui-password         задать / сменить пароль страницы агента
  svyazest-agent service install|uninstall|start|stop
  svyazest-agent version

Флаги (после команды):
  --data-dir DIR     каталог данных (по умолчанию %s; или SVYAZEST_DATA)
  --listen ADDR      адрес страницы агента (по умолчанию %s)
  --xray PATH        xray-core 26.3.27 (по умолчанию рядом с программой или /usr/lib/svyazest-agent/xray)
  --xray-next PATH   xray-core 26.10.31 (xray-next)
  --api URL          сервер (по умолчанию %s)
  --modem-iface IF   считать модемом этот интерфейс (для проверки и особых случаев)
  --verbose          подробный журнал в консоль
`

type opts struct {
	dataDir, listen, xray, xrayNext, apiBase, modemIface string
	verbose, passwordStdin                               bool
}

func defaultListen() string {
	if v := os.Getenv("SVYAZEST_LISTEN"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return "127.0.0.1:8780"
	}
	return "0.0.0.0:8780"
}

func parseFlags(name string, args []string) (*opts, []string) {
	o := &opts{}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&o.dataDir, "data-dir", "", "каталог данных")
	fs.StringVar(&o.listen, "listen", "", "адрес страницы агента")
	fs.StringVar(&o.xray, "xray", "", "путь к xray")
	fs.StringVar(&o.xrayNext, "xray-next", "", "путь к xray-next")
	fs.StringVar(&o.apiBase, "api", "", "адрес сервера")
	fs.StringVar(&o.modemIface, "modem-iface", "", "интерфейс модема")
	fs.BoolVar(&o.verbose, "verbose", false, "подробный журнал")
	fs.BoolVar(&o.passwordStdin, "password-stdin", false, "пароль из stdin (ui-password)")
	fs.Usage = func() { printUsage() }
	_ = fs.Parse(args)
	if o.apiBase == "" {
		o.apiBase = os.Getenv("SVYAZEST_API")
	}
	if o.apiBase == "" {
		o.apiBase = api.DefaultBase
	}
	o.apiBase = strings.TrimRight(o.apiBase, "/")
	return o, fs.Args()
}

func printUsage() {
	fmt.Fprint(os.Stderr, colorUsage(fmt.Sprintf(usage, version, store.DefaultDir(), defaultListen(), api.DefaultBase)))
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		switch {
		case svc.IsService():
			args = []string{"run"}
		case menuAvailable():
			// 1.0.3: без команды в терминале — интерактивное меню.
			checks.AppVersion = version
			o, _ := parseFlags("menu", nil)
			if err := cmdMenu(o); err != nil {
				fmt.Fprintln(os.Stderr, cRed("Ошибка:"), err)
				os.Exit(1)
			}
			return
		default:
			printUsage()
			os.Exit(2)
		}
	}
	checks.AppVersion = version
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "run":
		o, _ := parseFlags(cmd, rest)
		err = cmdRun(o)
	case "login":
		o, _ := parseFlags(cmd, rest)
		err = cmdLogin(o)
	case "status":
		o, _ := parseFlags(cmd, rest)
		err = cmdStatus(o)
	case "logout":
		o, _ := parseFlags(cmd, rest)
		err = cmdLogout(o)
	case "ui-password":
		o, _ := parseFlags(cmd, rest)
		err = cmdPassword(o)
	case "service":
		if len(rest) == 0 {
			err = errors.New("укажите: service install | uninstall | start | stop")
			break
		}
		o, _ := parseFlags(cmd, rest[1:])
		err = cmdService(rest[0], o, rest[1:])
	case "version", "--version", "-v":
		fmt.Printf("svyazest-agent %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "%s %s\n\n", cRed("неизвестная команда:"), cmd)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, cRed("Ошибка:"), err)
		os.Exit(1)
	}
}

// ---- shared setup ----

func openStore(o *opts) (*store.Store, error) {
	st, err := store.Open(o.dataDir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%v\nНужны права администратора: запустите через sudo (Linux) или от имени администратора (Windows)", err)
		}
		return nil, err
	}
	return st, nil
}

// findBinary: flag, then next to the executable, then /usr/lib/svyazest-agent.
func findBinary(flagVal, name string) string {
	if flagVal != "" {
		return flagVal
	}
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	var cands []string
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		cands = append(cands, filepath.Join(filepath.Dir(self), exe))
	}
	if runtime.GOOS != "windows" {
		cands = append(cands, "/usr/lib/svyazest-agent/"+exe)
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if len(cands) > 0 {
		return cands[len(cands)-1]
	}
	return exe
}

// simpleControl: used only if netbind gives no Control (should not happen).
type simpleControl struct{ hc *http.Client }

func (s simpleControl) Do(r *http.Request) (*http.Response, error) { return s.hc.Do(r) }
func (s simpleControl) LastRoute() string                          { return "default" }
func (s simpleControl) SetModem(string)                            {}

func newControl(modemIface string) netbind.Control {
	if c := netbind.NewControl(modemIface); c != nil {
		return c
	}
	return simpleControl{hc: &http.Client{Timeout: 70 * time.Second}}
}

// guessModem for one-shot commands: --modem-iface, settings, else the
// first connected modem (so the control channel avoids it).
func guessModem(o *opts, st *store.Store) string {
	if o.modemIface != "" {
		return o.modemIface
	}
	if v := st.Settings().ModemIface; v != "" {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, _ := modem.Detect(ctx)
	for _, m := range list {
		if m.Connected {
			return m.Interface
		}
	}
	return ""
}

func resolveListen(o *opts, st *store.Store) string {
	l := o.listen
	if l == "" {
		l = st.Settings().UIListen
	}
	if l == "" {
		l = defaultListen()
	}
	if strings.HasPrefix(l, ":") {
		l = "0.0.0.0" + l
	}
	return l
}

// ---- run ----

func cmdRun(o *opts) error {
	st, err := openStore(o)
	if err != nil {
		return err
	}
	j := store.OpenJournal(st.Dir)
	defer j.Close()
	if !(runtime.GOOS == "windows" && svc.IsService()) {
		j.Console = os.Stderr // systemd / docker logs / terminal
	}
	j.Verbose = o.verbose

	dev := api.DeviceInfo(version, st.RandomMachineID)
	modemIface := o.modemIface
	if modemIface == "" {
		modemIface = st.Settings().ModemIface
	}
	ctl := newControl(modemIface)
	cl := api.New(o.apiBase, ctl, version, dev.Platform, st.Token)
	key, err := st.DeviceKey()
	if err != nil {
		j.Warn("ключ устройства: %v — подпись запросов недоступна", err)
	} else {
		cl.SetKey(key)
	}
	xray := findBinary(o.xray, "xray")
	xrayNext := findBinary(o.xrayNext, "xray-next")
	r := runner.New(runner.Config{
		Store: st, Journal: j, API: cl, Ctl: ctl, Device: dev,
		XrayPath: xray, XrayNextPath: xrayNext, APIBase: o.apiBase,
		ModemOverride: o.modemIface, Version: version,
	})
	login := &runner.Login{API: cl, Store: st, Meta: dev, Logf: j.Info, OnDone: r.Wake}
	listen := resolveListen(o, st)
	localTok := st.NewLocalToken()
	srv := ui.New(ui.Options{
		Runner: r, Store: st, Journal: j, API: cl, Login: login, Device: dev, Version: version,
		Listen: listen, XrayPath: xray, XrayNextPath: xrayNext, LocalToken: localTok,
		Restart: func() {
			if !svc.RestartSelf() {
				r.Restart()
			}
		},
		SetAutostart: func(on bool) error {
			if !svc.IsService() {
				return nil // not installed as a service: only the setting is kept
			}
			return svc.SetAutostart(on)
		},
		ServiceState: svc.State,
	})

	run := func(ctx context.Context) {
		go func() {
			for ctx.Err() == nil {
				announceUI(j, st, srv, listen)
				if err := srv.ListenAndServe(ctx); err != nil {
					j.Warn("страница агента на %s не запустилась: %v — повтор через 30 с", listen, err)
					select {
					case <-ctx.Done():
					case <-time.After(30 * time.Second):
					}
					continue
				}
				return
			}
		}()
		r.Run(ctx)
	}

	if runtime.GOOS == "windows" && svc.IsService() {
		return svc.RunService(run)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run(ctx)
	return nil
}

func lanAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || virtualIfName(i.Name) {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// virtualIfName: bridges/tunnels nobody on the LAN can open the page
// through (docker0 172.17.0.1 used to be listed first under Docker).
func virtualIfName(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "lxc", "cni", "flannel", "tun", "tap", "wg", "tailscale", "zt", "vmnet", "vboxnet"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// statusCmd: how to run `status` here -- inside the container the partner
// types it on the host through docker exec.
func statusCmd() string {
	if store.InDocker() {
		return "sudo docker exec svyazest-agent svyazest-agent status"
	}
	return "sudo svyazest-agent status"
}

func pageURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{"http://" + listen + "/"}
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		var out []string
		for _, ip := range lanAddrs() {
			out = append(out, "http://"+ip+":"+port+"/")
		}
		return append(out, "http://127.0.0.1:"+port+"/")
	}
	return []string{"http://" + net.JoinHostPort(host, port) + "/"}
}

func announceUI(j *store.Journal, st *store.Store, srv *ui.Server, listen string) {
	urls := pageURLs(listen)
	j.Info("страница агента: %s", strings.Join(urls, " , "))
	if !srv.LoopbackOnly() && st.Settings().UIPassHash == "" {
		st.SetupCode(true)
		j.Info("свой пароль страницы не задан: вход на страницу — по коду настройки, его показывает «%s»", statusCmd())
	}
}

// ---- talking to the running agent ----

func localCall(o *opts, st *store.Store, method, path string, out any) error {
	tok := st.LocalToken()
	if tok == "" {
		return errors.New("служба не запущена")
	}
	listen := resolveListen(o, st)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	req, _ := http.NewRequest(method, "http://"+net.JoinHostPort(host, port)+path, strings.NewReader("{}"))
	req.Header.Set("X-Local-Token", tok)
	req.Header.Set("X-Svyazest", "1")
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("служба не отвечает")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("служба ответила HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ---- status ----

type liveStatus struct {
	Status runner.Status  `json:"status"`
	About  map[string]any `json:"about"`
}

// agentInfo: what «status» and the menu show.
type agentInfo struct {
	loggedIn bool
	executor string
	svcState string // running | stopped | unknown | "" (no service)
	live     *liveStatus
	liveErr  error
	listen   string
	passSet  bool
	loop     bool // page only on this computer
}

func gatherInfo(o *opts, st *store.Store) agentInfo {
	inf := agentInfo{svcState: svc.State()}
	if d, ok := st.Device(); ok {
		inf.loggedIn, inf.executor = true, d.ExecutorID
	}
	var live liveStatus
	if err := localCall(o, st, http.MethodGet, "/ui/api/local/status", &live); err != nil {
		inf.liveErr = err
	} else {
		inf.live = &live
	}
	inf.listen = resolveListen(o, st)
	host, _, _ := net.SplitHostPort(inf.listen)
	ip := net.ParseIP(host)
	inf.loop = host == "localhost" || (ip != nil && ip.IsLoopback())
	inf.passSet = st.Settings().UIPassHash != ""
	return inf
}

func svcStateText(s string) string {
	switch s {
	case "running":
		return cOK("работает")
	case "stopped":
		return cBad("остановлена")
	case "unknown":
		return cWarn("неизвестно")
	}
	return ""
}

func modemText(m *runner.ModemView) string {
	if m == nil {
		return cWarn("не найден") + cDim(" — подключите USB-модем или телефон в режиме модема")
	}
	// 1.0.5: only what is known (a phone over USB reports no operator/signal).
	parts := []string{cBold(m.Model) + " (" + m.Interface + ")"}
	if m.Operator != "" {
		parts = append(parts, "оператор "+cBold(m.Operator)+ifStr(m.OperatorByIP, cDim(" (по IP)")))
	}
	if m.Country != "" {
		parts = append(parts, "страна "+strings.ToUpper(m.Country)+ifStr(m.CountryByIP && !m.OperatorByIP, cDim(" (по IP)")))
	}
	if m.TechLabel != "" {
		parts = append(parts, "сеть "+m.TechLabel)
	}
	if m.Signal >= 0 {
		parts = append(parts, fmt.Sprintf("сигнал %d%%", m.Signal))
	}
	if m.Connected {
		parts = append(parts, cOK("подключён"))
	} else {
		parts = append(parts, cBad("нет сети"))
	}
	out := strings.Join(parts, ", ")
	switch {
	case m.Source == "tether" && m.Operator == "":
		out += cDim(" — оператор и сигнал телефон по USB не сообщает")
	}
	return out
}

// stateText (1.0.7): «заявка отклонена», «работает — …» — from a small
// letter; for the application only the headline (the «Заявка» row says
// what to do).
func stateText(s runner.Status) string {
	out := toneColor(s.Tone, lowerFirst(s.Headline))
	if s.Text != "" && s.Code != "moderation" {
		out += cDim(" — " + lowerFirst(s.Text))
	}
	return out
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || r == utf8.RuneError {
		return s
	}
	// «IPv6 …», «VPN …»: an abbreviation stays as it is
	if r2, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(r2) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

func ifStr(c bool, s string) string {
	if c {
		return s
	}
	return ""
}

// moderationText (1.0.6): the partner application as the Android app shows
// it; "" when there is nothing to say.
func moderationText(s runner.Status, withApproved bool) string {
	switch s.Moderation {
	case "pending":
		return cWarn("на модерации") + cDim(" — проверяем заявку (страна, регион, оператор); пока её не одобрят, задания не приходят. Ответ придёт в Telegram-бот «Связь Есть?»")
	case "rejected":
		out := cBad("отклонена")
		if r := strings.TrimSpace(s.ModerationReason); r != "" {
			out += ": " + r
		}
		return out + cDim(" — исправьте данные и подайте заявку заново в Telegram-боте «Связь Есть?»; до этого задания не приходят")
	case "approved":
		if withApproved {
			return cOK("одобрена")
		}
	}
	return ""
}

// outdatedText (1.0.6): the server no longer gives jobs to this version.
func outdatedText(s runner.Status) string {
	if !s.AppOutdated {
		return ""
	}
	need := ""
	if s.MinApp != "" {
		need = " (нужна " + s.MinApp + " или новее)"
	}
	return cBad("нужно обновить агента") + " — версия " + version + " больше не поддерживается" + need +
		". Пока не обновитесь, задания приходить не будут"
}

// titleLine: «Связь Есть? · агент 1.0.6» — the same in the menu and status.
func titleLine() string {
	return cTitle("Связь Есть?") + cDim(" · агент ") + cBold(version)
}

func row(label, value string) {
	fmt.Printf("  %-18s%s\n", label+":", value)
}

func cmdStatus(o *opts) error {
	st, err := openStore(o)
	if err != nil {
		return err
	}
	dev := api.DeviceInfo(version, st.RandomMachineID)
	inf := gatherInfo(o, st)
	fmt.Println()
	fmt.Println("  " + titleLine())
	fmt.Println()
	row("Компьютер", fmt.Sprintf("%s, %s (%s/%s)", dev.Model, dev.OSVersion, runtime.GOOS, runtime.GOARCH))
	row("Каталог данных", st.Dir)
	if inf.loggedIn {
		row("Вход", cOK("выполнен")+" (партнёр "+cBold(inf.executor)+")")
	} else {
		row("Вход", cWarn("не выполнен")+" — «"+cGreen("svyazest-agent login")+"» или на странице агента")
	}
	if t := svcStateText(inf.svcState); t != "" {
		row("Служба", t)
	}
	if inf.live == nil {
		row("Состояние", cBad(inf.liveErr.Error()))
	} else {
		s := inf.live.Status
		row("Состояние", stateText(s))
		row("Модем", modemText(s.Modem))
		if t := moderationText(s, true); t != "" {
			row("Заявка", t)
		}
		if t := outdatedText(s); t != "" {
			row("Обновление", t)
		}
		row("Связь с сервером", serverText(s))
		if s.Whitelist.Verdict != "" {
			row("Белые списки", s.Whitelist.Label)
		}
		row("Выполнено", fmt.Sprintf("%s %s", cBold(fmt.Sprint(s.JobsTotal)), cDim(fmt.Sprintf("(%d за этот запуск)", s.JobsDone))))
		if s.LastError != "" {
			row("Последняя ошибка", cRed(s.LastError))
		}
	}
	urls := pageURLs(inf.listen)
	for i := range urls {
		urls[i] = cCyan(urls[i])
	}
	row("Страница агента", strings.Join(urls, "  "))
	switch {
	case inf.passSet:
		row("Пароль страницы", cOK("задан")+" (сменить: «"+cGreen("svyazest-agent ui-password")+"»)")
	case inf.loop:
		row("Пароль страницы", "не нужен — страница открыта только на этом компьютере")
	default:
		row("Пароль страницы", cWarn(st.SetupCode(true))+cDim(" — код настройки; свой пароль: «svyazest-agent ui-password»"))
	}
	return nil
}

// serverText (1.0.9): is the server answering, by which path, how long ago.
func serverText(s runner.Status) string {
	sv := s.Server
	if sv.At == nil {
		return cDim("ещё не было запросов")
	}
	ago := cDim(" (" + agoShort(*sv.At) + ")")
	if !sv.OK {
		return cBad("нет связи") + cDim(" — "+nz(sv.Error, "сервер недоступен")) + ago
	}
	via := ""
	if s.Route != "" {
		via = cDim(" через " + routeText(s.Route))
	}
	return cOK("есть") + via + ago
}

func agoShort(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "только что"
	case d < time.Minute:
		return fmt.Sprintf("%d с назад", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	}
}

// routeText: "ethernet:eth0" -> "Ethernet · eth0", as on the page.
func routeText(r string) string {
	if r == "cellular" {
		return "модем (запасной путь)"
	}
	kind, name, _ := strings.Cut(r, ":")
	k := map[string]string{"ethernet": "Ethernet", "wifi": "Wi-Fi", "default": "по умолчанию", "other": "сеть"}[kind]
	if k == "" {
		k = kind
	}
	if name != "" {
		return k + " · " + name
	}
	return k
}

func nz(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- logout ----

func cmdLogout(o *opts) error {
	st, err := openStore(o)
	if err != nil {
		return err
	}
	if _, ok := st.Device(); !ok {
		fmt.Println(cYellow("Вход и так не выполнен."))
		return nil
	}
	if err := localCall(o, st, http.MethodPost, "/ui/api/local/logout", nil); err != nil {
		if err := st.ClearDevice(); err != nil {
			return err
		}
	}
	fmt.Println(cOK("Выход выполнен:") + " задания на этот компьютер больше не придут, пока вы не войдёте снова.")
	return nil
}

// ---- login (terminal) ----

func cmdLogin(o *opts) error {
	st, err := openStore(o)
	if err != nil {
		return err
	}
	if d, ok := st.Device(); ok {
		fmt.Printf("%s (партнёр %s). Чтобы войти другим аккаунтом: «%s».\n", cOK("Вход уже выполнен"), cBold(d.ExecutorID), cGreen("svyazest-agent logout"))
		return nil
	}
	dev := api.DeviceInfo(version, st.RandomMachineID)
	ctl := newControl(guessModem(o, st))
	cl := api.New(o.apiBase, ctl, version, dev.Platform, st.Token)
	login := &runner.Login{API: cl, Store: st, Meta: dev}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println(cTitle("Вход в «Связь Есть?»"))
	fmt.Println()
	ls, err := login.Start(ctx, nil)
	if err != nil {
		return err
	}
	if c, err := qr.Encode(ls.URL); err == nil {
		fmt.Print(c.Terminal())
	}
	fmt.Println()
	fmt.Println("Отсканируйте QR-код телефоном или откройте ссылку, в боте нажмите «Start»:")
	fmt.Println("  " + cCyan(ls.URL))
	fmt.Println()
	fmt.Println("Или введите " + cBold("код из бота") + " (кнопка «Код для входа») и нажмите Enter.")
	fmt.Println(cDim("Ожидаем подтверждение… (Ctrl+C — отмена)"))

	codes := linesCtx(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			login.Cancel()
			fmt.Println("\n" + cYellow("Вход отменён."))
			return nil
		case code, ok := <-codes:
			if !ok {
				codes = nil // stdin closed: wait for Telegram only
				continue
			}
			if code = strings.TrimSpace(code); code == "" {
				continue
			}
			if err := login.Pair(ctx, code); err != nil {
				fmt.Println(cRed("Код не подошёл:"), err)
				continue
			}
			return loginDone(o, st)
		case <-tick.C:
			s := login.State()
			switch s.Status {
			case "ok":
				return loginDone(o, st)
			case "expired", "error":
				return errors.New(nz(s.Error, "вход не удался"))
			}
		}
	}
}

func loginDone(o *opts, st *store.Store) error {
	d, _ := st.Device()
	fmt.Printf("\n%s партнёр %s.\n", cOK("Вход выполнен:"), cBold(d.ExecutorID))
	if err := localCall(o, st, http.MethodGet, "/ui/api/local/status", nil); err == nil {
		fmt.Println("Служба уже работает и подхватит вход сама.")
		// 1.0.6: what the server says about the application, like the
		// Android app right after the sign-in.
		for i := 0; i < 16; i++ {
			var live liveStatus
			if localCall(o, st, http.MethodGet, "/ui/api/local/status", &live) == nil && live.Status.LoggedIn && live.Status.Moderation != "" {
				if t := moderationText(live.Status, true); t != "" {
					fmt.Println("Заявка партнёра: " + t)
				}
				if t := outdatedText(live.Status); t != "" {
					fmt.Println(t)
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		fmt.Println(cYellow("Запустите службу:") + " " + cGreen(startHint()))
	}
	return nil
}

func startHint() string {
	if runtime.GOOS == "windows" {
		return "svyazest-agent service start"
	}
	// systemd / OpenRC / runit / SysV (1.0.2, internal/svc).
	return svc.StartHint()
}

// ---- page password ----

func cmdPassword(o *opts) error {
	st, err := openStore(o)
	if err != nil {
		return err
	}
	var pass string
	if o.passwordStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return err
		}
		pass = strings.TrimRight(string(b), "\r\n")
	} else {
		p1, err := readPassword("Новый пароль страницы (от 8 символов): ")
		if err != nil {
			return err
		}
		p2, err := readPassword("Ещё раз: ")
		if err != nil {
			return err
		}
		if p1 != p2 {
			return errors.New("пароли не совпадают")
		}
		pass = p1
	}
	if len([]rune(pass)) < 8 {
		return errors.New("пароль — не короче 8 символов")
	}
	if err := ui.SetPassword(st, pass); err != nil {
		return err
	}
	fmt.Println(cOK("Пароль страницы сохранён.") + " Открытые сеансы страницы закрыты.")
	return nil
}

// ---- service ----

func cmdService(action string, o *opts, raw []string) error {
	switch action {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		args := []string{"run"}
		add := func(k, v string) {
			if v != "" {
				args = append(args, "--"+k, v)
			}
		}
		add("data-dir", o.dataDir)
		add("listen", o.listen)
		add("xray", o.xray)
		add("xray-next", o.xrayNext)
		if o.apiBase != api.DefaultBase {
			add("api", o.apiBase)
		}
		add("modem-iface", o.modemIface)
		if err := svc.Install(exe, args); err != nil {
			return err
		}
		fmt.Printf("Служба %s установлена (автозапуск, перезапуск при сбое).\n", svc.Name)
		if err := svc.Start(); err != nil {
			return fmt.Errorf("служба установлена, но не запустилась: %w", err)
		}
		fmt.Println("Служба запущена. Страница агента: http://127.0.0.1:8780/")
		return nil
	case "uninstall":
		if err := svc.Uninstall(); err != nil {
			return err
		}
		fmt.Printf("Служба %s удалена. Данные остались в %s.\n", svc.Name, store.DefaultDir())
		return nil
	case "start":
		if err := svc.Start(); err != nil {
			return err
		}
		fmt.Println(cOK("Служба запущена."))
		return nil
	case "stop":
		if err := svc.Stop(); err != nil {
			return err
		}
		fmt.Println(cYellow("Служба остановлена."))
		return nil
	}
	return fmt.Errorf("неизвестное действие: %s (install | uninstall | start | stop)\n%s", action, svc.Hint())
}
