package runner

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/modem"
	"svyazest-agent/internal/netbind"
)

type modemState struct {
	mu      sync.Mutex
	list    []modem.Info
	ifaces  []netbind.Iface
	at      time.Time
	err     string
	refresh chan struct{}
	chosen  string // interface of the last selected modem (for change logs)
	// 1.0.6: what the server made of the mobile network's IP on netIface
	// (a phone over USB tells no operator / country itself).
	netIface, netOperator, netCountry string
}

// modemWatch: modem.Detect about every 30 s (and on demand).
func (r *Runner) modemWatch(ctx context.Context) {
	r.modems.mu.Lock()
	if r.modems.refresh == nil {
		r.modems.refresh = make(chan struct{}, 1)
	}
	ch := r.modems.refresh
	r.modems.mu.Unlock()
	go r.modemHotplug(ctx, ch)
	for {
		r.detectModems(ctx)
		t := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-ch:
			t.Stop()
		}
	}
}

// modemHotplug (1.0.4): an interface appeared / went away / got an address
// (USB modem or phone plugged in, «USB-модем» switched on) -> look at once.
// Events come in bursts: wait 1.5 s of quiet, then look; ModemManager needs
// a few more seconds to probe a new stick — look again after 8 and 20 s.
func (r *Runner) modemHotplug(ctx context.Context, ch chan struct{}) {
	poke := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	var (
		mu       sync.Mutex
		quiet    *time.Timer
		followUp []*time.Timer
	)
	kick := func() {
		mu.Lock()
		defer mu.Unlock()
		if quiet != nil {
			quiet.Stop()
		}
		quiet = time.AfterFunc(1500*time.Millisecond, func() {
			poke()
			mu.Lock()
			for _, t := range followUp {
				t.Stop()
			}
			followUp = []*time.Timer{time.AfterFunc(8*time.Second, poke), time.AfterFunc(20*time.Second, poke)}
			mu.Unlock()
		})
	}
	if err := modem.WatchHotplug(ctx, kick); err != nil {
		r.j.Debug("слежение за подключением модемов недоступно: %v (поиск раз в 30 с)", err)
	}
}

// RefreshModems: re-detect now (UI «Обновить»), waits for the result.
func (r *Runner) RefreshModems(ctx context.Context) {
	r.detectModems(ctx)
}

func (r *Runner) detectModems(ctx context.Context) {
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	list, err := modem.Detect(dctx)
	cancel()
	ifaces, _ := netbind.List()
	r.modems.mu.Lock()
	prev := r.modems.list
	r.modems.list, r.modems.ifaces, r.modems.at = list, ifaces, time.Now()
	if err != nil {
		r.modems.err = shortErr(err)
	} else {
		r.modems.err = ""
	}
	r.modems.mu.Unlock()
	if !sameModems(prev, list) {
		if len(list) == 0 {
			r.j.Info("модемы: не найдено")
		}
		for _, m := range list {
			// 1.0.5: only what is known (a phone over USB reports no operator/signal).
			extra := ""
			if m.Operator != "" {
				extra += ", оператор " + m.Operator
			}
			if t := TechLabel(m.Tech); t != "" {
				extra += ", " + t
			}
			if m.SignalPct >= 0 {
				extra += ", сигнал " + signalLabel(m.SignalPct)
			}
			r.j.Info("модем: %s через %s (%s)%s, %s", nz(m.Model, "?"), m.Interface, m.Source, extra,
				ifs(m.Connected, "подключён")+ifs(!m.Connected, "не подключён"))
		}
	}
	if err != nil {
		r.diagOnce("modem_err", "поиск модемов: "+shortErr(err))
	}
	r.publishModem()
}

// publishModem: the current modem into the status (menu, page).
func (r *Runner) publishModem() {
	m, ok := r.currentModem()
	var v ModemView
	if ok {
		v = r.modemView(m)
	}
	r.update(func(s *Status) {
		if ok {
			s.Modem = &v
		} else {
			s.Modem = nil
		}
	})
}

// setNetInfo (1.0.6): the heartbeat reply told the operator / country of
// the mobile network's IP on this interface.
func (r *Runner) setNetInfo(iface, operator, country string) {
	r.modems.mu.Lock()
	changed := r.modems.netIface != iface || r.modems.netOperator != operator || r.modems.netCountry != country
	r.modems.netIface, r.modems.netOperator, r.modems.netCountry = iface, operator, country
	r.modems.mu.Unlock()
	if !changed {
		return
	}
	if operator != "" || country != "" {
		r.j.Info("мобильная сеть по IP: %s", strings.Join(nonEmpty(ifs(operator != "", "оператор "+operator),
			ifs(country != "", "страна "+strings.ToUpper(country))), ", "))
	}
	r.publishModem()
}

// netInfo: operator / country by IP for this interface ("" when unknown).
func (r *Runner) netInfo(iface string) (operator, country string) {
	r.modems.mu.Lock()
	defer r.modems.mu.Unlock()
	if iface == "" || r.modems.netIface != iface {
		return "", ""
	}
	return r.modems.netOperator, r.modems.netCountry
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sameModems(a, b []modem.Info) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Interface != y.Interface || x.Connected != y.Connected || x.Operator != y.Operator || x.Model != y.Model {
			return false
		}
	}
	return true
}

// Modems: last detection result + all interfaces (for «Ещё → Модем»).
func (r *Runner) Modems() ([]modem.Info, []netbind.Iface, time.Time, string) {
	r.modems.mu.Lock()
	defer r.modems.mu.Unlock()
	all := append([]netbind.Iface(nil), r.modems.ifaces...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return append([]modem.Info(nil), r.modems.list...), all, r.modems.at, r.modems.err
}

// currentModem: --modem-iface, else the interface chosen in settings, else
// the first connected modem (else the first one found, not connected).
func (r *Runner) currentModem() (modem.Info, bool) {
	r.modems.mu.Lock()
	list := r.modems.list
	ifaces := r.modems.ifaces
	r.modems.mu.Unlock()

	want := r.cfg.ModemOverride
	if want == "" {
		want = r.st.Settings().ModemIface
	}
	var m modem.Info
	ok := false
	if want != "" {
		for _, x := range list {
			if x.Interface == want {
				m, ok = x, true
				break
			}
		}
		if !ok {
			// Any interface may be named "the modem" (a router's LAN port,
			// an unknown dongle): treat it as up when the OS says so.
			for _, ifc := range ifaces {
				if ifc.Name == want {
					m = modem.Info{Interface: want, Model: "Интерфейс " + want, Source: "manual", SignalPct: -1,
						Connected: ifc.Up && len(ifc.Addrs) > 0}
					ok = true
					break
				}
			}
		}
		if !ok {
			m = modem.Info{Interface: want, Model: "Интерфейс " + want, Source: "manual", SignalPct: -1}
			ok = true
		}
	} else {
		for _, x := range list {
			if x.Connected {
				m, ok = x, true
				break
			}
		}
		if !ok && len(list) > 0 {
			m, ok = list[0], true
		}
	}
	if ok && m.Country == "" {
		m.Country = countryFromMCC(m.MCCMNC)
	}
	r.modems.mu.Lock()
	changed := ok && m.Interface != r.modems.chosen
	if ok {
		r.modems.chosen = m.Interface
	}
	r.modems.mu.Unlock()
	if changed {
		r.j.Info("выбран модем: %s (%s)%s", m.Interface, nz(m.Model, "?"), ifs(want != "", ", выбран вручную"))
	}
	return m, ok
}

func (r *Runner) modemView(m modem.Info) ModemView {
	v := ModemView{
		Interface: m.Interface, Model: m.Model, Operator: m.Operator, MCCMNC: m.MCCMNC, Country: m.Country,
		Tech: m.Tech, TechLabel: TechLabel(m.Tech), Signal: m.SignalPct, Source: m.Source, Connected: m.Connected,
		Manual: m.Source == "manual",
	}
	op, cc := r.netInfo(m.Interface)
	if v.Operator == "" && op != "" {
		v.Operator, v.OperatorByIP = op, true
	}
	if v.Country == "" && cc != "" {
		v.Country, v.CountryByIP = cc, true
	}
	return v
}

func TechLabel(t string) string {
	switch strings.ToLower(t) {
	case "lte", "4g":
		return "4G (LTE)"
	case "5gnr", "5g", "nr":
		return "5G"
	case "umts", "hspa", "hspa+", "3g", "wcdma":
		return "3G"
	case "gsm", "edge", "gprs", "2g":
		return "2G"
	case "":
		return ""
	}
	return strings.ToUpper(t)
}

func signalLabel(p int) string {
	if p < 0 {
		return "—"
	}
	return itoa(p) + "%"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// countryFromMCC: fallback when modem did not fill Country.
func countryFromMCC(mccmnc string) string {
	if len(mccmnc) < 3 {
		return ""
	}
	switch mccmnc[:3] {
	case "250":
		return "ru"
	case "255":
		return "ua"
	case "257":
		return "by"
	case "401":
		return "kz"
	case "282":
		return "ge"
	case "283":
		return "am"
	case "400":
		return "az"
	case "434":
		return "uz"
	case "436":
		return "tj"
	case "437":
		return "kg"
	case "438":
		return "tm"
	case "259":
		return "md"
	case "246":
		return "lt"
	case "247":
		return "lv"
	case "248":
		return "ee"
	case "260":
		return "pl"
	case "262":
		return "de"
	case "244":
		return "fi"
	case "286":
		return "tr"
	case "460":
		return "cn"
	case "432":
		return "ir"
	}
	return ""
}
