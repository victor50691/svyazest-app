//go:build linux

package main

// 1.0.3: «svyazest-agent» без команды в терминале — меню с цифрами вместо
// того, чтобы помнить команды: состояние, вход/выход, служба, журнал,
// пароль страницы, обновление.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"svyazest-agent/internal/store"
	"svyazest-agent/internal/svc"
)

// 1.0.8: files come from GitHub Releases (rolling release «desktop»).
const desktopBase = "https://github.com/victor50691/svyazest-app/releases/download/desktop"

func downloadBase() string {
	if v := os.Getenv("SVYAZEST_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return desktopBase
}

// menuAvailable: без команды и за терминалом сидит человек.
func menuAvailable() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }

func inDocker() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// ensureRoot: данные агента доступны только root — перезапуск через sudo.
func ensureRoot() error {
	if os.Geteuid() == 0 || os.Getenv("SVYAZEST_DATA") != "" {
		return nil
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("нужны права администратора: запустите меню от root")
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "svyazest-agent"
	}
	fmt.Println(cYellow("Нужны права администратора — запускаю через sudo…"))
	return syscall.Exec(sudo, append([]string{"sudo", exe}, os.Args[1:]...), os.Environ())
}

// ---- свежая версия на сайте (по SHA256SUMS раздачи) ----

var (
	latestMu   sync.Mutex
	latestVer  string
	latestDone bool
)

// startLatestCheck: at once and then every 10 minutes while the menu is
// open (1.0.6) — a new version shows up on the screen by itself.
func startLatestCheck() {
	go func() {
		for {
			v := fetchLatest()
			latestMu.Lock()
			if v != "" || !latestDone {
				latestVer = v
			}
			latestDone = true
			latestMu.Unlock()
			time.Sleep(10 * time.Minute)
		}
	}()
}

func latest() (string, bool) {
	latestMu.Lock()
	defer latestMu.Unlock()
	return latestVer, latestDone
}

func fetchLatest() string {
	hc := &http.Client{Timeout: 8 * time.Second}
	resp, err := hc.Get(downloadBase() + "/SHA256SUMS")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	best := ""
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		// svyazest-agent_1.0.3_arm64.deb
		if !strings.HasPrefix(name, "svyazest-agent_") || !strings.HasSuffix(name, ".deb") {
			continue
		}
		parts := strings.Split(name, "_")
		if len(parts) == 3 && (best == "" || cmpVer(parts[1], best) > 0) {
			best = parts[1]
		}
	}
	return best
}

// cmpVer: 1.0.10 > 1.0.9.
func cmpVer(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// ---- экран ----

func clearScreen() {
	if colorOn {
		fmt.Print("\x1b[H\x1b[2J")
	}
}

func pause(text string) {
	if text == "" {
		text = "Любая клавиша — вернуться в меню"
	}
	fmt.Print("\n" + cDim(text) + " ")
	readKey()
	fmt.Println()
}

// confirm: y — да (в русской раскладке та же клавиша — «н»), любая
// другая клавиша — нет.
func confirm(q string) bool {
	fmt.Print(q + " " + cDim("[y/n]") + " ")
	k, _ := readKey()
	yes := false
	switch strings.ToLower(k) {
	case "y", "н":
		yes = true
	}
	if yes {
		fmt.Println(cGreen("да"))
	} else {
		fmt.Println("нет")
	}
	return yes
}

// menuScreen (1.0.6): the whole menu as text — drawn, and compared every
// second with a fresh one: when the modem, the service or the state change,
// the screen is redrawn without a key press.
func menuScreen(st *store.Store, inf agentInfo, items []menuItem) string {
	var b strings.Builder
	printMenuHeader(&b, st, inf)
	fmt.Fprintln(&b)
	for i, it := range items {
		fmt.Fprintf(&b, "  %s  %s\n", sgr("1;36", fmt.Sprintf("%d", i+1)), it.label)
	}
	fmt.Fprintf(&b, "  %s  %s\n", sgr("1;36", "0"), "Выход")
	return b.String()
}

func printMenuHeader(w io.Writer, st *store.Store, inf agentInfo) {
	title := titleLine()
	if v, _ := latest(); v != "" && cmpVer(v, version) > 0 {
		title += "   " + cWarn("доступна версия "+v)
	}
	mrow := func(label, value string) { fmt.Fprintf(w, "  %-11s %s\n", label, value) }
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+title)
	fmt.Fprintln(w)
	if inf.loggedIn {
		mrow("Вход", cOK("выполнен")+", партнёр "+cBold(inf.executor))
	} else {
		mrow("Вход", cWarn("не выполнен")+cDim(" — войдите: пункт 1"))
	}
	if t := svcStateText(inf.svcState); t != "" {
		mrow("Служба", t)
	}
	if inf.live == nil {
		mrow("Состояние", cBad(inf.liveErr.Error()))
	} else {
		s := inf.live.Status
		mrow("Состояние", stateText(s))
		mrow("Модем", modemText(s.Modem))
		if t := moderationText(s, false); t != "" {
			mrow("Заявка", t)
		}
		if t := outdatedText(s); t != "" {
			if inDocker() {
				t += " — обновите контейнер"
			} else {
				t += " — пункт «Обновить агента»"
			}
			mrow("Обновление", t)
		}
		mrow("Выполнено", cBold(fmt.Sprint(s.JobsTotal)))
		if s.LastError != "" {
			mrow("Ошибка", cRed(s.LastError))
		}
	}
	urls := pageURLs(inf.listen)
	if len(urls) > 0 {
		mrow("Страница", cCyan(urls[0]))
	}
	if !inf.passSet && !inf.loop {
		// 1.0.7: the setup code is the page password until an own one is set.
		mrow("Пароль", cWarn(st.SetupCode(true))+cDim(" — код настройки, им входят на страницу"))
	}
}

type menuItem struct {
	label string
	run   func() (note string, wait bool, err error)
}

// waitService: после запуска служба отвечает не сразу.
func waitService(o *opts, st *store.Store) {
	for i := 0; i < 10; i++ {
		if localCall(o, st, http.MethodGet, "/ui/api/local/status", nil) == nil {
			return
		}
		time.Sleep(700 * time.Millisecond)
	}
}

func buildItems(o *opts, st *store.Store, inf agentInfo) []menuItem {
	var items []menuItem
	if !inf.loggedIn {
		items = append(items, menuItem{cOK("Войти в аккаунт партнёра") + cDim(" (QR-код, ссылка или код из бота)"), func() (string, bool, error) {
			return "", true, cmdLogin(o)
		}})
	}
	items = append(items,
		menuItem{"Подробное состояние", func() (string, bool, error) {
			clearScreen()
			return "", true, cmdStatus(o)
		}},
		menuItem{"Журнал работы", func() (string, bool, error) {
			return "", false, showJournal(st)
		}},
	)
	passLabel := "Задать свой пароль страницы агента"
	if inf.passSet {
		passLabel = "Сменить пароль страницы агента"
	}
	items = append(items, menuItem{passLabel, func() (string, bool, error) {
		return "", true, cmdPassword(o)
	}})
	switch inf.svcState {
	case "running":
		items = append(items,
			menuItem{"Перезапустить службу", func() (string, bool, error) {
				fmt.Println(cDim("Перезапускаю службу…"))
				_ = svc.Stop()
				if err := svc.Start(); err != nil {
					return "", true, err
				}
				waitService(o, st)
				return cOK("Служба перезапущена."), false, nil
			}},
			menuItem{"Остановить службу", func() (string, bool, error) {
				if !confirm("Остановить службу? Если вы выключите, то задания приходить не будут.") {
					return "", false, nil
				}
				if err := svc.Stop(); err != nil {
					return "", true, err
				}
				return cYellow("Служба остановлена.") + " Запустить снова — пункт «Запустить службу».", false, nil
			}},
		)
	case "stopped", "unknown":
		items = append(items, menuItem{cOK("Запустить службу"), func() (string, bool, error) {
			fmt.Println(cDim("Запускаю службу…"))
			if err := svc.Start(); err != nil {
				return "", true, err
			}
			_ = svc.SetAutostart(true)
			waitService(o, st)
			return cOK("Служба запущена."), false, nil
		}})
	}
	if !inDocker() {
		label := "Обновить агента"
		v, _ := latest()
		newer := v != "" && cmpVer(v, version) > 0
		if newer {
			label += " до " + v
		}
		switch {
		case inf.live != nil && inf.live.Status.AppOutdated:
			label = cBad(label) + cDim(" (обязательно)")
		case newer:
			label = cWarn(label)
		}
		items = append(items, menuItem{label, func() (string, bool, error) {
			return "", true, doUpdate()
		}})
	}
	if inf.loggedIn {
		items = append(items, menuItem{"Выйти из аккаунта", func() (string, bool, error) {
			if !confirm("Выйти из аккаунта? Задания на этот компьютер перестанут приходить.") {
				return "", false, nil
			}
			return "", true, cmdLogout(o)
		}})
	}
	return items
}

func cmdMenu(o *opts) error {
	if err := ensureRoot(); err != nil {
		return err
	}
	st, err := openStore(o)
	if err != nil {
		return err
	}
	startLatestCheck()
	// 1.0.6: leaving the menu takes it off the screen.
	leave := func() {
		if colorOn {
			clearScreen()
		} else {
			fmt.Println()
		}
	}
	note := ""
	for {
		inf := gatherInfo(o, st)
		items := buildItems(o, st, inf)
		screen := menuScreen(st, inf, items)
		clearScreen()
		fmt.Print(screen)
		if note != "" {
			fmt.Println()
			fmt.Println("  " + note)
		}
		fmt.Print("\n  " + cDim("Нажмите цифру пункта") + " ")
		key, ok, changed := keyOrChange(o, st, screen)
		if changed {
			continue // the note stays until a key is pressed
		}
		note = ""
		if !ok {
			leave()
			return nil
		}
		switch strings.ToLower(key) {
		case "\n", "\r", " ":
			continue
		case "0", "q", "й", "\x03", "\x04", "\x1b":
			leave()
			return nil
		}
		n, err := strconv.Atoi(key)
		if err != nil || n < 1 || n > len(items) {
			if strconv.IsPrint([]rune(key)[0]) {
				note = cRed("Нет пункта «"+key+"»") + " — нажмите цифру из списка."
			}
			continue
		}
		fmt.Print(cBold(key))
		fmt.Println()
		msg, wait, err := items[n-1].run()
		if err != nil {
			fmt.Println(cRed("Ошибка:"), err)
			wait = true
		}
		if wait {
			pause("")
		} else {
			note = msg
		}
	}
}

// keyOrChange: a key press, or changed=true as soon as the menu would look
// different (modem plugged in / out, service restarted, new state…).
func keyOrChange(o *opts, st *store.Store, screen string) (key string, ok, changed bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flag atomic.Bool
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			inf := gatherInfo(o, st)
			if ctx.Err() == nil && menuScreen(st, inf, buildItems(o, st, inf)) != screen {
				flag.Store(true)
				cancel()
				return
			}
		}
	}()
	key, ok = readKeyCtx(ctx)
	if !ok && flag.Load() {
		return "", false, true
	}
	return key, ok, false
}

// ---- журнал ----

const journalTime = "2006-01-02T15:04:05.000Z07:00"

func fmtJournal(ln string) string {
	f := strings.SplitN(ln, " ", 3)
	if len(f) != 3 {
		return ln
	}
	ts := f[0]
	if t, err := time.Parse(journalTime, f[0]); err == nil {
		ts = t.Local().Format("02.01 15:04:05")
	}
	msg := f[2]
	switch f[1] {
	case "WARN":
		msg = cYellow(msg)
	case "ERROR":
		msg = cRed(msg)
	case "DEBUG":
		msg = cDim(msg)
	}
	return cDim(ts) + "  " + msg
}

func showJournal(st *store.Store) error {
	path := filepath.Join(st.Dir, "journal.log")
	clearScreen()
	fmt.Println(cTitle("Журнал агента") + cDim("  — новые строки появляются сами; любая клавиша — назад в меню"))
	fmt.Println()
	b, _ := os.ReadFile(path)
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		fmt.Println(cDim("(пока пусто)"))
	} else {
		lines := strings.Split(text, "\n")
		if len(lines) > 30 {
			lines = lines[len(lines)-30:]
		}
		for _, ln := range lines {
			fmt.Println(fmtJournal(ln))
		}
	}
	off := int64(len(b))
	partial := ""
	t := time.NewTicker(time.Second)
	defer t.Stop()
	in := make(chan struct{})
	go func() {
		readKey()
		close(in)
	}()
	for {
		select {
		case <-in:
			return nil
		case <-t.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			if fi.Size() < off { // журнал перевернулся (1 МБ)
				off, partial = 0, ""
			}
			if fi.Size() == off {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			_, _ = f.Seek(off, io.SeekStart)
			data, _ := io.ReadAll(io.LimitReader(f, 2<<20))
			f.Close()
			off += int64(len(data))
			chunk := partial + string(data)
			cut := strings.LastIndex(chunk, "\n")
			if cut < 0 {
				partial = chunk
				continue
			}
			partial = chunk[cut+1:]
			for _, ln := range strings.Split(chunk[:cut], "\n") {
				fmt.Println(fmtJournal(ln))
			}
		}
	}
}

// ---- обновление ----

func doUpdate() error {
	if v, _ := latest(); v != "" && cmpVer(v, version) <= 0 {
		if !confirm("Установлена последняя версия (" + version + "). Всё равно переустановить?") {
			return nil
		}
	}
	url := downloadBase() + "/install.sh"
	var script string
	switch {
	case haveCmd("curl"):
		script = "curl -fsSL '" + url + "' | sh"
	case haveCmd("wget"):
		script = "wget -qO- '" + url + "' | sh"
	default:
		return errors.New("нужен curl или wget")
	}
	fmt.Println(cDim("Обновляю: " + script))
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "SVYAZEST_BASE="+downloadBase())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("обновление не удалось (%v)", err)
	}
	pause("Любая клавиша — открыть меню новой версии")
	exe := "/usr/bin/svyazest-agent"
	if _, err := os.Stat(exe); err != nil {
		if exe, err = os.Executable(); err != nil {
			return nil
		}
	}
	return syscall.Exec(exe, []string{exe}, os.Environ())
}

func haveCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
