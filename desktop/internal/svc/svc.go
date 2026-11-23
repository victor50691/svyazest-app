// Package svc: running as a system service. Windows: the SCM service
// «SvyazEstAgent» (golang.org/x/sys/windows/svc), installed with automatic
// start and restart on failure. Linux: systemd, OpenRC, runit or SysV init
// (svc_unix.go) -- the service files ship with the package / install.sh,
// here start / stop / state, hints and «Автозапуск».
package svc

const (
	Name        = "SvyazEstAgent"
	DisplayName = "Связь Есть? — агент"
	Description = "Агент «Связь Есть?»: выполняет проверки DPI//Checker через мобильный модем."
	UnitName    = "svyazest-agent.service"
)
