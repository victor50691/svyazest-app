package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/api"
	"svyazest-agent/internal/store"
)

// Login: the Telegram sign-in (auth/start -> the partner opens the link ->
// auth/poll until 'ok') and the manual code (/pair). Shared by the web UI
// and `svyazest-agent login`.
type Login struct {
	API    *api.Client
	Store  *store.Store
	Meta   api.DeviceMeta
	Logf   func(format string, a ...any)
	OnDone func() // after a successful sign-in (runner.Wake)

	mu      sync.Mutex
	state   LoginState
	cancel  context.CancelFunc
	pollGen int
}

type LoginState struct {
	Status    string    `json:"status"` // idle | starting | waiting | ok | expired | error
	URL       string    `json:"url,omitempty"`
	Code      string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func (l *Login) State() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *Login) set(fn func(s *LoginState)) {
	l.mu.Lock()
	fn(&l.state)
	l.mu.Unlock()
}

// Start: new Telegram link; polling runs in the background until it
// expires, is cancelled or succeeds. onChange is called on every change.
func (l *Login) Start(ctx context.Context, onChange func()) (LoginState, error) {
	l.Cancel()
	l.set(func(s *LoginState) { *s = LoginState{Status: "starting"} })
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := l.API.AuthStart(sctx, l.Meta)
	if err != nil {
		msg := friendlyLoginErr(err)
		l.set(func(s *LoginState) { *s = LoginState{Status: "error", Error: msg} })
		return l.State(), errors.New(msg)
	}
	exp := time.Duration(st.ExpiresIn) * time.Second
	if exp <= 0 {
		exp = 10 * time.Minute
	}
	pctx, pcancel := context.WithTimeout(context.Background(), exp+5*time.Second)
	l.mu.Lock()
	l.pollGen++
	gen := l.pollGen
	l.cancel = pcancel
	l.state = LoginState{Status: "waiting", URL: st.URL, Code: st.Code, ExpiresAt: time.Now().Add(exp)}
	l.mu.Unlock()
	if l.Logf != nil {
		l.Logf("вход через Telegram: ссылка выдана, ждём подтверждения в боте")
	}
	go l.poll(pctx, gen, st.Code, onChange)
	return l.State(), nil
}

func (l *Login) poll(ctx context.Context, gen int, code string, onChange func()) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			if l.pollGen == gen && l.state.Status == "waiting" {
				l.state = LoginState{Status: "expired", Error: "Ссылка для входа истекла. Попробуйте ещё раз."}
			}
			l.mu.Unlock()
			if onChange != nil {
				onChange()
			}
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		r, err := l.API.AuthPoll(rctx, code)
		cancel()
		l.mu.Lock()
		stale := l.pollGen != gen
		l.mu.Unlock()
		if stale {
			return
		}
		if err != nil {
			if e, ok := api.AsError(err); ok && e.Status == 403 {
				l.set(func(s *LoginState) { *s = LoginState{Status: "error", Error: e.Message} })
				if onChange != nil {
					onChange()
				}
				return
			}
			continue // transient: keep polling until the link expires
		}
		switch r.Status {
		case "ok":
			if err := l.save(r.Pair); err != nil {
				l.set(func(s *LoginState) {
					*s = LoginState{Status: "error", Error: "Не удалось сохранить вход: " + err.Error()}
				})
			} else {
				l.set(func(s *LoginState) { *s = LoginState{Status: "ok"} })
			}
			if onChange != nil {
				onChange()
			}
			return
		case "expired":
			l.set(func(s *LoginState) {
				*s = LoginState{Status: "expired", Error: "Ссылка для входа истекла. Попробуйте ещё раз."}
			})
			if onChange != nil {
				onChange()
			}
			return
		}
	}
}

func (l *Login) Cancel() {
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	l.pollGen++
	if l.state.Status == "waiting" || l.state.Status == "starting" {
		l.state = LoginState{Status: "idle"}
	}
	l.mu.Unlock()
}

// Pair: the 8-character code from the bot («Код для входа»).
func (l *Login) Pair(ctx context.Context, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, " ", "")
	if code == "" {
		return errors.New("Введите код")
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := l.API.Pair(pctx, code, l.Meta)
	if err != nil {
		if e, ok := api.AsError(err); ok && e.Status == 404 {
			return errors.New("Код неверный или истёк. Получите новый в боте.")
		}
		return errors.New(friendlyLoginErr(err))
	}
	l.Cancel()
	if err := l.save(res); err != nil {
		return err
	}
	l.set(func(s *LoginState) { *s = LoginState{Status: "ok"} })
	return nil
}

func (l *Login) save(p api.PairResult) error {
	err := l.Store.SaveDevice(store.Device{Token: p.DeviceToken, ExecutorID: string(p.ExecutorID), DeviceID: int64(p.DeviceID)})
	if err != nil {
		return err
	}
	l.Store.UpdateState(func(s *store.State) { s.SignedOutNote = "" })
	if l.Logf != nil {
		l.Logf("вход выполнен: партнёр %s, устройство %d", p.ExecutorID, int64(p.DeviceID))
	}
	if l.OnDone != nil {
		l.OnDone()
	}
	return nil
}

func friendlyLoginErr(err error) string {
	if e, ok := api.AsError(err); ok {
		if e.Status == 429 {
			return "Слишком много попыток. Подождите минуту."
		}
		if e.Message != "" && e.Status < 500 {
			return e.Message
		}
		return "Сервер временно недоступен. Попробуйте ещё раз."
	}
	return "Не удалось подключиться к серверу. Проверьте интернет и попробуйте ещё раз."
}
