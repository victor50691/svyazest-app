package runner

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/checks"
	"svyazest-agent/internal/store"
)

// diagLog = Android DiagLog + DiagConsent: the detailed log goes to the
// server ONLY while support's request is 'accepted' by the partner (answer
// in the web UI). Lines are already masked by the journal.
type diagLog struct {
	r        *Runner
	mu       sync.Mutex
	id       int64
	queue    []api.DiagLine
	kick     chan struct{}
	pending  *api.Request // waiting for the partner's answer
	notified int64
}

func newDiagLog(r *Runner) *diagLog {
	d := &diagLog{r: r, kick: make(chan struct{}, 1)}
	r.j.SetDiagSink(d.add)
	return d
}

func (d *diagLog) activeID() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.id
}

func (d *diagLog) add(t time.Time, src, msg string) {
	d.mu.Lock()
	if d.id == 0 {
		d.mu.Unlock()
		return
	}
	d.queue = append(d.queue, api.DiagLine{T: t.UnixMilli(), S: src, M: msg})
	if len(d.queue) > 20000 {
		d.queue = d.queue[len(d.queue)-20000:]
	}
	n := len(d.queue)
	d.mu.Unlock()
	if n >= 100 {
		select {
		case d.kick <- struct{}{}:
		default:
		}
	}
}

func (d *diagLog) setActive(id int64) {
	d.mu.Lock()
	was := d.id
	d.id = id
	if id == 0 {
		d.queue = nil
	}
	d.mu.Unlock()
	if was == id {
		return
	}
	d.r.st.UpdateState(func(s *store.State) { s.DiagActiveID = id })
	if id > 0 {
		d.r.j.Info("сбор логов для поддержки включён (запрос #%d)", id)
		d.snapshot()
	} else if was > 0 {
		d.r.j.Info("сбор логов для поддержки выключен")
	}
	d.r.Notify()
}

func (d *diagLog) snapshot() {
	r := d.r
	main, next := checks.XrayVersions(checks.Env{XrayPath: r.cfg.XrayPath, XrayNextPath: r.cfg.XrayNextPath})
	r.j.Debug("устройство: %s, %s (%s/%s, Go %s); агент %s; xray-core %s / %s",
		r.cfg.Device.Model, r.cfg.Device.OSVersion, runtime.GOOS, runtime.GOARCH, runtime.Version(), r.cfg.Version, nz(main, "—"), nz(next, "—"))
	mods, ifaces, _, merr := r.Modems()
	for _, m := range mods {
		r.j.Debug("модем: %s %s оператор=%s mccmnc=%s страна=%s сеть=%s сигнал=%d источник=%s подключён=%v",
			m.Interface, m.Model, m.Operator, m.MCCMNC, m.Country, m.Tech, m.SignalPct, m.Source, m.Connected)
	}
	if merr != "" {
		r.j.Debug("поиск модемов: ошибка %s", merr)
	}
	for _, i := range ifaces {
		r.j.Debug("интерфейс: %s тип=%s up=%v адресов=%d шлюз=%v DNS=%d", i.Name, i.Kind, i.Up, len(i.Addrs), i.Gateway.IsValid(), len(i.DNS))
	}
	r.j.Debug("связь с сервером: %s", nz(r.api.Route(), "ещё не было запросов"))
}

// sync: bring the consent state in line with the server's diag field.
func (d *diagLog) sync(ctx context.Context, req *api.Request) {
	if req == nil {
		d.mu.Lock()
		d.pending = nil
		d.mu.Unlock()
		if d.activeID() > 0 {
			d.flush(ctx)
			d.setActive(0)
		}
		d.r.update(func(s *Status) { s.Diag = nil })
		return
	}
	id := int64(req.ID)
	d.r.update(func(s *Status) { c := *req; s.Diag = &c })
	switch req.Status {
	case "pending":
		d.mu.Lock()
		d.pending = req
		first := d.notified != id
		d.notified = id
		d.mu.Unlock()
		if first {
			d.r.j.Warn("поддержка просит разрешить сбор логов (запрос #%d)%s — ответьте в веб-интерфейсе агента",
				id, ifs(req.Reason != "", ", причина: «"+req.Reason+"»"))
		}
		if d.activeID() > 0 && d.activeID() != id {
			d.setActive(0)
		}
	case "accepted":
		d.mu.Lock()
		d.pending = nil
		d.mu.Unlock()
		d.setActive(id)
	default:
		d.setActive(0)
	}
}

// Decide: the partner's answer from the web UI.
func (r *Runner) DiagDecide(ctx context.Context, accept bool) (string, error) {
	r.mu.RLock()
	req := r.status.Diag
	r.mu.RUnlock()
	if req == nil || req.Status != "pending" {
		return "", fmt.Errorf("нет запроса на сбор логов")
	}
	id := int64(req.ID)
	status, err := r.api.DiagDecision(ctx, id, accept)
	if err != nil {
		return "", err
	}
	r.j.Info("сбор логов (запрос #%d): %s", id, ifs(accept, "разрешён")+ifs(!accept, "отклонён"))
	c := *req
	c.Status = status
	r.diag.sync(ctx, &c)
	return status, nil
}

// DiagStop: «Остановить сбор логов».
func (r *Runner) DiagStop(ctx context.Context) error {
	id := r.diag.activeID()
	if id == 0 {
		r.mu.RLock()
		if r.status.Diag != nil {
			id = int64(r.status.Diag.ID)
		}
		r.mu.RUnlock()
	}
	if id == 0 {
		return nil
	}
	r.diag.flush(ctx)
	if err := r.api.DiagStop(ctx, id); err != nil {
		return err
	}
	r.diag.setActive(0)
	r.update(func(s *Status) { s.Diag = nil })
	return nil
}

func (d *diagLog) run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	snapAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
		if d.activeID() == 0 {
			continue
		}
		if time.Since(snapAt) > 5*time.Minute {
			snapAt = time.Now()
			d.snapshot()
		}
		d.flush(ctx)
	}
}

func (d *diagLog) flush(ctx context.Context) {
	for {
		d.mu.Lock()
		id := d.id
		if id == 0 || len(d.queue) == 0 {
			d.mu.Unlock()
			return
		}
		n := len(d.queue)
		if n > 1000 {
			n = 1000
		}
		batch := append([]api.DiagLine(nil), d.queue[:n]...)
		d.mu.Unlock()
		fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		active, err := d.r.api.DiagLogs(fctx, id, batch)
		cancel()
		if err != nil {
			return // lines wait for the next tick
		}
		d.mu.Lock()
		if d.id == id && len(d.queue) >= n {
			d.queue = d.queue[n:]
		}
		d.mu.Unlock()
		if !active {
			d.setActive(0)
			d.r.update(func(s *Status) { s.Diag = nil })
			return
		}
	}
}
