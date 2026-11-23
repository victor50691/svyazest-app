//go:build !windows

package svc

// Linux service managers (1.0.2): systemd, OpenRC (Alpine, Gentoo, Artix),
// runit (Void, Artix) and SysV init scripts (Devuan, older systems). The
// service files themselves are put in place by the .deb / install.sh
// (packaging/{systemd,openrc,runit,sysv}); here: start, stop, autostart and
// state for whichever manager runs this system.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const serviceName = "svyazest-agent"

type manager int

const (
	mNone manager = iota
	mSystemd
	mOpenRC
	mRunit
	mSysV
)

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func have(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// runit: where this service's definition lives and where enabled services
// are linked (Void: /var/service, Artix: /run/runit/service, others /etc/service).
const runitDef = "/etc/sv/" + serviceName

func runitActiveDir() string {
	for _, d := range []string{"/var/service", "/run/runit/service", "/etc/service", "/service"} {
		if dirExists(d) {
			return d
		}
	}
	return ""
}

const sysvScript = "/etc/init.d/" + serviceName

// detect: the manager that runs this system AND has our service installed
// (systemd and OpenRC are told by the system itself).
func detect() manager {
	if dirExists("/run/systemd/system") && have("systemctl") {
		return mSystemd
	}
	if have("rc-service") && (dirExists("/run/openrc") || have("openrc-run")) {
		return mOpenRC
	}
	if have("sv") && dirExists(runitDef) && runitActiveDir() != "" {
		return mRunit
	}
	if fileExists(sysvScript) {
		return mSysV
	}
	return mNone
}

// IsService: started by a service manager -- systemd sets INVOCATION_ID,
// our OpenRC / runit / SysV scripts set SVYAZEST_SERVICE=1.
func IsService() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getenv("SVYAZEST_SERVICE") == "1"
}

// RunService is only meaningful on Windows; on Linux the caller runs in
// the foreground and the service manager supervises it.
func RunService(run func(ctx context.Context)) error {
	return errors.New("not a Windows service")
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// exitCode: 0 = success, -1 = could not run at all.
func exitCode(name string, args ...string) (int, string) {
	out, err := exec.Command(name, args...).Output()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	return -1, ""
}

func noManager() error {
	return errors.New("служба не установлена (контейнер?) — запускайте «svyazest-agent run» средствами вашей системы")
}

// Hint: how to manage the service on this system.
func Hint() string {
	switch detect() {
	case mOpenRC:
		return `Службой управляет OpenRC (ставится install.sh):
  sudo rc-service svyazest-agent start          # запустить
  sudo rc-service svyazest-agent restart        # перезапустить
  sudo rc-service svyazest-agent stop           # остановить
  sudo rc-update add svyazest-agent default     # автозапуск (del — убрать)
  tail -f /var/log/svyazest-agent.log           # журнал`
	case mRunit:
		return `Службой управляет runit (ставится install.sh):
  sudo sv up svyazest-agent                     # запустить
  sudo sv restart svyazest-agent                # перезапустить
  sudo sv down svyazest-agent                   # остановить
  автозапуск: нет файла /etc/sv/svyazest-agent/down
  tail -f /var/log/svyazest-agent/current       # журнал`
	case mSysV:
		return `Служба — скрипт /etc/init.d/svyazest-agent (ставится install.sh):
  sudo /etc/init.d/svyazest-agent start         # запустить
  sudo /etc/init.d/svyazest-agent restart       # перезапустить
  sudo /etc/init.d/svyazest-agent stop          # остановить
  tail -f /var/log/svyazest-agent.log           # журнал`
	}
	return `На Linux службой управляет systemd, unit ставится пакетом:
  sudo systemctl enable --now svyazest-agent   # включить и запустить
  sudo systemctl restart svyazest-agent        # перезапустить
  sudo systemctl stop svyazest-agent           # остановить
  sudo systemctl disable svyazest-agent        # убрать из автозапуска
  journalctl -u svyazest-agent -f              # журнал
OpenRC, runit и SysV init: служба ставится install.sh.
В Docker: docker run ... --restart unless-stopped (см. документацию пакета).`
}

// StartHint: one line for «start the service» on this system.
func StartHint() string {
	switch detect() {
	case mSystemd:
		return "sudo systemctl enable --now svyazest-agent"
	case mOpenRC:
		return "sudo rc-service svyazest-agent start (автозапуск: sudo rc-update add svyazest-agent default)"
	case mRunit:
		return "sudo sv up svyazest-agent"
	case mSysV:
		return "sudo /etc/init.d/svyazest-agent start"
	}
	return "svyazest-agent run (или поставьте службу: curl -fsSL https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh)"
}

func Install(exe string, args []string) error { return errors.New(Hint()) }
func Uninstall() error                        { return errors.New(Hint()) }

func withHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%v\n\n%s", err, Hint())
}

func Start() error {
	switch detect() {
	case mSystemd:
		return withHint(run("systemctl", "start", UnitName))
	case mOpenRC:
		return withHint(run("rc-service", serviceName, "start"))
	case mRunit:
		if err := runitLink(); err != nil {
			return withHint(err)
		}
		return withHint(run("sv", "up", runitSvc()))
	case mSysV:
		return withHint(run(sysvScript, "start"))
	}
	return withHint(noManager())
}

func Stop() error {
	switch detect() {
	case mSystemd:
		return withHint(run("systemctl", "stop", UnitName))
	case mOpenRC:
		return withHint(run("rc-service", serviceName, "stop"))
	case mRunit:
		return withHint(run("sv", "down", runitSvc()))
	case mSysV:
		return withHint(run(sysvScript, "stop"))
	}
	return withHint(noManager())
}

// runit: the enabled service = a link in the active dir; «down» file in the
// definition = do not start it by itself (our «Автозапуск» off).
func runitSvc() string { return filepath.Join(runitActiveDir(), serviceName) }

func runitLink() error {
	link := runitSvc()
	if _, err := os.Lstat(link); err == nil {
		return nil
	}
	return os.Symlink(runitDef, link)
}

// SetAutostart: start at boot or not (best effort).
func SetAutostart(on bool) error {
	switch detect() {
	case mSystemd:
		if on {
			return run("systemctl", "enable", UnitName)
		}
		return run("systemctl", "disable", UnitName)
	case mOpenRC:
		if on {
			return run("rc-update", "add", serviceName, "default")
		}
		return run("rc-update", "del", serviceName, "default")
	case mRunit:
		down := filepath.Join(runitDef, "down")
		if on {
			if err := os.Remove(down); err != nil && !os.IsNotExist(err) {
				return err
			}
			return runitLink()
		}
		return os.WriteFile(down, nil, 0o644)
	case mSysV:
		switch {
		case have("update-rc.d"):
			if on {
				return run("update-rc.d", serviceName, "defaults")
			}
			return run("update-rc.d", "-f", serviceName, "remove")
		case have("chkconfig"):
			if on {
				return run("chkconfig", serviceName, "on")
			}
			return run("chkconfig", serviceName, "off")
		}
		return errors.New("нет update-rc.d / chkconfig — автозапуск настройте средствами системы")
	}
	return noManager()
}

// State: "running" | "stopped" | "unknown" | "" (no service installed).
func State() string {
	switch detect() {
	case mSystemd:
		if lo, _ := exec.Command("systemctl", "show", "-p", "LoadState", "--value", UnitName).Output(); strings.TrimSpace(string(lo)) != "loaded" {
			return "" // unit not installed
		}
		out, _ := exec.Command("systemctl", "is-active", UnitName).Output()
		switch strings.TrimSpace(string(out)) {
		case "active":
			return "running"
		case "inactive", "failed":
			return "stopped"
		}
		return "unknown"
	case mOpenRC:
		if !fileExists(sysvScript) {
			return ""
		}
		switch code, _ := exitCode("rc-service", serviceName, "status"); code {
		case 0:
			return "running"
		case 3:
			return "stopped"
		}
		return "unknown"
	case mRunit:
		if _, err := os.Lstat(runitSvc()); err != nil {
			return "stopped" // defined, not enabled
		}
		_, out := exitCode("sv", "status", runitSvc())
		switch {
		case strings.HasPrefix(out, "run:"):
			return "running"
		case strings.HasPrefix(out, "down:"):
			return "stopped"
		}
		return "unknown"
	case mSysV:
		switch code, _ := exitCode(sysvScript, "status"); code {
		case 0:
			return "running"
		case 3:
			return "stopped"
		}
		return "unknown"
	}
	return ""
}

// RestartSelf: ask the service manager to restart us (UI «Перезапустить
// службу»); false = the caller restarts its own loop. Only systemd can do
// it without killing the very request that asked (--no-block); under the
// other managers the in-process restart does the same job.
func RestartSelf() bool {
	if os.Getenv("INVOCATION_ID") == "" || detect() != mSystemd {
		return false
	}
	return exec.Command("systemctl", "--no-block", "restart", UnitName).Run() == nil
}
