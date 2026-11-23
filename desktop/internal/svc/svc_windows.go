//go:build windows

package svc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type handler struct{ run func(ctx context.Context) }

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run(ctx)
	}()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				st <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		case <-done:
			// The loop ended by itself: report a failure so the SCM's
			// recovery actions restart us.
			return false, 1
		}
	}
}

func RunService(run func(ctx context.Context)) error {
	return svc.Run(Name, &handler{run: run})
}

func Install(exe string, args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (запустите от имени администратора): %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("служба %s уже установлена", Name)
	}
	s, err := m.CreateService(Name, exe, mgr.Config{
		DisplayName:  DisplayName,
		Description:  Description,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 24*3600)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	return nil
}

func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (запустите от имени администратора): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return fmt.Errorf("служба %s не установлена", Name)
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	waitState(s, svc.Stopped, 20*time.Second)
	return s.Delete()
}

func waitState(s *mgr.Service, want svc.State, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		q, err := s.Query()
		if err != nil || q.State == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("нет доступа к диспетчеру служб (запустите от имени администратора): %w", err)
	}
	s, err := m.OpenService(Name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("служба %s не установлена («svyazest-agent service install»)", Name)
	}
	return m, s, nil
}

func Start() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return nil
}

func Stop() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	waitState(s, svc.Stopped, 25*time.Second)
	return nil
}

// StartHint: one line for «start the service» (main.go startHint; the
// Windows branch there does not call it, kept for the shared API).
func StartHint() string { return "svyazest-agent service start" }

func Hint() string {
	return "svyazest-agent service install | uninstall | start | stop (от имени администратора)"
}

func SetAutostart(on bool) error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	if on {
		cfg.StartType = mgr.StartAutomatic
	} else {
		cfg.StartType = mgr.StartManual
	}
	return s.UpdateConfig(cfg)
}

func State() string {
	m, s, err := open()
	if err != nil {
		return ""
	}
	defer m.Disconnect()
	defer s.Close()
	q, err := s.Query()
	if err != nil {
		return "unknown"
	}
	switch q.State {
	case svc.Running, svc.StartPending:
		return "running"
	case svc.Stopped, svc.StopPending:
		return "stopped"
	}
	return "unknown"
}

// RestartSelf: under the SCM a detached "sc stop/start" is awkward; the
// caller just restarts its own loop.
func RestartSelf() bool {
	_ = exec.Command
	return false
}
