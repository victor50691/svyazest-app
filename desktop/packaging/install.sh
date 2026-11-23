#!/bin/sh
# «Связь Есть?» для Linux / Raspberry Pi — установка и обновление одной командой:
#
#   curl -fsSL https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh
#
# Флаги (через «sh -s --»):
#   --no-docker         ставить пакетом и службой, даже если есть Docker
#   --docker            только Docker (ошибка, если его нет)
#   --no-modemmanager   не ставить ModemManager
#   --version X         другая версия (по умолчанию — @VERSION@)
#   -v, --verbose       подробный вывод вместо полоски прогресса
#
#   curl -fsSL https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh -s -- --no-docker
#
# В терминале видна только полоска прогресса и итог; всё остальное — в
# журнале /var/log/svyazest-agent-install.log (при ошибке — его хвост).
#
# Есть Docker — ставит образ и docker-compose в /opt/svyazest-agent.
# Нет — .deb (Debian, Ubuntu, Raspberry Pi OS, Devuan) или архив (любой
# другой Linux: Fedora, Arch, openSUSE, Alpine, Void, Gentoo ...) и службу
# той системы запуска, что стоит: systemd, OpenRC, runit или SysV init.
# Программы собраны статически — библиотеки дистрибутива не нужны.
# ModemManager (для USB-модемов) ставится сам, если его нет.
# Повторный запуск = обновление; вход партнёра и журнал сохраняются.
set -eu

BASE="${SVYAZEST_BASE:-https://github.com/victor50691/svyazest-app/releases/download/desktop}"
VERSION="@VERSION@"
MODE=auto
WITH_MM=1
OPT=/opt/svyazest-agent

VERBOSE="${SVYAZEST_VERBOSE:-0}"
LOG=/var/log/svyazest-agent-install.log
TMP=""
TICK=""

# fd 3 / 4 — настоящий терминал: полоска, итог и ошибки идут туда, а обычный
# вывод команд (stdout/stderr) в режиме полоски уходит в журнал.
exec 3>&1 4>&2
P_ON=0

say() { printf '%s\n' "$*"; }
# out: итоговые строки — всегда на экран.
out() { printf '%s\n' "$*" >&3; }
# warn: важное предупреждение — на экран после полоски (и в журнал).
warn() {
	say "$*"
	if [ "$P_ON" = 1 ] && [ -n "$TMP" ]; then printf '%s\n' "$*" >> "$TMP/warn"; fi
	return 0
}
die() {
	tick_stop
	[ "$P_ON" = 1 ] && printf '\n' >&3
	printf 'Ошибка: %s\n' "$*" >&4
	exit 1
}

# ---------------------------------------------------------------- прогресс
# Только ASCII: не всякий SSH-клиент показывает Unicode.
BAR_W=30
bar() { # процент
	[ "$P_ON" = 1 ] || return 0
	_bp=$1
	[ "$_bp" -le 100 ] || _bp=100
	_bn=$((_bp * BAR_W / 100))
	_bi=0
	_bs=""
	while [ $_bi -lt $BAR_W ]; do
		if [ $_bi -lt $_bn ]; then _bs="$_bs#"; else _bs="$_bs-"; fi
		_bi=$((_bi + 1))
	done
	printf '\r[%s] %3d%%' "$_bs" "$_bp" >&3
}

# tick ОТ ДО [файл размер]: пока идёт шаг, полоска ползёт от ОТ к ДО (не
# доходя): по размеру скачанного файла или плавно, всё медленнее.
tick() {
	[ "$P_ON" = 1 ] || return 0
	_tf=$1 _tt=$2 _tfile="${3:-}" _tsize="${4:-0}"
	(
		q=$((_tf * 100))
		while :; do
			if [ -n "$_tfile" ] && [ "$_tsize" -gt 0 ]; then
				c=0
				[ -f "$_tfile" ] && c=$(wc -c < "$_tfile" 2>/dev/null || echo 0)
				q=$(( (_tf * 100) + (_tt - _tf) * 100 * c / _tsize ))
			else
				q=$((q + (_tt * 100 - q) / 15))
			fi
			p=$((q / 100))
			[ $p -lt $_tt ] || p=$((_tt - 1))
			bar $p
			sleep 1
		done
	) >/dev/null 2>&1 &
	TICK=$!
}
tick_stop() {
	if [ -n "$TICK" ]; then
		kill "$TICK" 2>/dev/null || true
		wait "$TICK" 2>/dev/null || true
		TICK=""
	fi
	return 0
}
# prog ОТ [ДО [файл размер]]: шаг дошёл до ОТ; дальше ползти к ДО.
prog() {
	tick_stop
	bar "$1"
	if [ $# -ge 2 ]; then tick "$@"; fi
	return 0
}

on_exit() {
	rc=$?
	tick_stop
	if [ $rc -ne 0 ] && [ "$P_ON" = 1 ]; then
		printf '\n' >&3
		if [ -s "$LOG" ]; then
			printf 'Установка не завершена. Последние строки журнала %s:\n' "$LOG" >&4
			tail -n 15 "$LOG" >&4 2>/dev/null || true
		fi
	fi
	[ -z "$TMP" ] || rm -rf "$TMP"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

while [ $# -gt 0 ]; do
	case "$1" in
		-v|--verbose) VERBOSE=1 ;;
		--no-docker) MODE=native ;;
		--docker) MODE=docker ;;
		--no-modemmanager) WITH_MM=0 ;;
		--version) shift; [ $# -gt 0 ] || die "после --version нужна версия"; VERSION="$1" ;;
		--version=*) VERSION="${1#--version=}" ;;
		-h|--help) sed -n '2,24p' "$0" 2>/dev/null || true; exit 0 ;;
		*) die "неизвестный флаг: $1" ;;
	esac
	shift
done

[ "$(id -u)" = "0" ] || die "нужны права root: curl -fsSL $BASE/install.sh | sudo sh"
[ "$(uname -s)" = "Linux" ] || die "этот установщик — для Linux. Для Windows скачайте $BASE/svyazest-agent-windows-setup.exe"

TMP="$(mktemp -d)"

# Полоска — когда вывод идёт в терминал; в файл/лог и с -v — подробный текст.
if [ "$VERBOSE" != 1 ] && [ -t 3 ]; then
	if : > "$LOG" 2>/dev/null; then
		chmod 0600 "$LOG" 2>/dev/null || true
		P_ON=1
		exec >>"$LOG" 2>&1
		say "$(date 2>/dev/null) установка «Связь Есть?» $VERSION"
		bar 0
	fi
fi

fetch() { # url file
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "нужен curl или wget"
	fi
}

# SHA256SUMS публикуется рядом с файлами: если он есть, файл обязан совпасть.
verify() { # file name
	if [ ! -s "$TMP/SHA256SUMS" ]; then
		fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" 2>/dev/null || : > "$TMP/SHA256SUMS"
	fi
	# Без awk (его нет в минимальном openSUSE и др.): строки «хеш  имя».
	want=""
	while read -r h n; do
		if [ "$n" = "$2" ] || [ "$n" = "*$2" ]; then want="$h"; fi
	done < "$TMP/SHA256SUMS"
	if [ -n "$want" ] && command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum "$1")"
		got="${got%% *}"
		[ "$got" = "$want" ] || die "контрольная сумма $2 не совпала — файл повреждён, попробуйте ещё раз"
	fi
}

# Размер файла на сервере (для полоски); 0 — неизвестен.
remote_size() {
	_rs=0
	if command -v curl >/dev/null 2>&1; then
		curl -fsSIL --connect-timeout 10 "$1" 2>/dev/null | tr -d '\r' > "$TMP/head" || true
		while read -r k v; do
			case "$k" in [Cc][Oo][Nn][Tt][Ee][Nn][Tt]-[Ll][Ee][Nn][Gg][Tt][Hh]:) _rs="$v" ;; esac
		done < "$TMP/head"
	fi
	case "$_rs" in ''|*[!0-9]*) _rs=0 ;; esac
	printf '%s' "$_rs"
}

download() { # name ОТ ДО -> path (полоска идёт ОТ..ДО по мере скачивания)
	say "Скачиваю $1 ..." >&2
	_ds="$(remote_size "$BASE/$1")"
	tick "$2" "$3" "$TMP/$1" "$_ds"
	fetch "$BASE/$1" "$TMP/$1" || die "не удалось скачать $BASE/$1"
	tick_stop
	verify "$TMP/$1" "$1"
	printf '%s' "$TMP/$1"
}

# Архитектура пользовательского окружения (на Raspberry Pi OS 32-bit ядро
# бывает 64-битным: uname -m = aarch64, а программы — armhf).
arch=""
if command -v dpkg >/dev/null 2>&1; then
	case "$(dpkg --print-architecture)" in
		amd64) arch=amd64 ;;
		arm64) arch=arm64 ;;
		armhf) arch=armv7 ;;
	esac
fi
if [ -z "$arch" ]; then
	case "$(uname -m)" in
		x86_64|amd64) arch=amd64 ;;
		aarch64|arm64) arch=arm64 ;;
		armv7l|armv8l) arch=armv7 ;;
		armv6l) die "ARMv6 (Raspberry Pi Zero / 1) не поддерживается — нужен Raspberry Pi 2 или новее" ;;
		*) die "архитектура $(uname -m) не поддерживается (есть amd64, arm64, armv7)" ;;
	esac
fi

have_docker=0
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	have_docker=1
fi
if [ "$MODE" = docker ] && [ $have_docker = 0 ]; then
	die "Docker не найден или не запущен"
fi
if [ "$MODE" = auto ]; then
	if [ $have_docker = 1 ]; then MODE=docker; else MODE=native; fi
fi

ip_addr() {
	a=""
	if command -v ip >/dev/null 2>&1; then
		# «1.1.1.1 via … dev eth0 src 192.168.1.5 …» -> слово после «src».
		set -- $(ip -4 route get 1.1.1.1 2>/dev/null)
		while [ $# -gt 1 ]; do
			if [ "$1" = src ]; then a="$2"; break; fi
			shift
		done
	fi
	if [ -z "$a" ]; then
		set -- $(hostname -I 2>/dev/null)
		a="${1:-}"
	fi
	printf '%s' "$a"
}

# Система запуска служб.
runit_dir() {
	for d in /var/service /run/runit/service /etc/service /service; do
		[ -d "$d" ] && { printf '%s' "$d"; return 0; }
	done
	return 1
}
init_system() {
	if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then echo systemd
	elif command -v rc-service >/dev/null 2>&1 && { [ -d /run/openrc ] || command -v openrc-run >/dev/null 2>&1; }; then echo openrc
	elif command -v sv >/dev/null 2>&1 && runit_dir >/dev/null; then echo runit
	elif [ -d /etc/init.d ]; then echo sysv
	else echo none
	fi
}
INIT="$(init_system)"

put() { # path mode  (stdin -> file, atomically)
	mkdir -p "$(dirname "$1")"
	cat > "$1.new" && chmod "$2" "$1.new" && mv -f "$1.new" "$1"
}

# Остановить и убрать из автозапуска службу установки без Docker (любой системы запуска).
disable_native() {
	case "$INIT" in
		systemd)
			if systemctl is-enabled svyazest-agent.service >/dev/null 2>&1 || systemctl is-active svyazest-agent.service >/dev/null 2>&1; then
				say "Отключаю службу прежней установки (данные остаются в /var/lib/svyazest-agent)."
				systemctl disable --now svyazest-agent.service >/dev/null 2>&1 || true
			fi ;;
		openrc)
			if [ -f /etc/init.d/svyazest-agent ]; then
				rc-service svyazest-agent stop >/dev/null 2>&1 || true
				rc-update del svyazest-agent default >/dev/null 2>&1 || true
			fi ;;
		runit)
			d="$(runit_dir)"
			if [ -e "$d/svyazest-agent" ]; then
				sv down "$d/svyazest-agent" >/dev/null 2>&1 || true
				rm -f "$d/svyazest-agent"
			fi ;;
		sysv)
			if [ -x /etc/init.d/svyazest-agent ]; then
				/etc/init.d/svyazest-agent stop >/dev/null 2>&1 || true
				if command -v update-rc.d >/dev/null 2>&1; then update-rc.d -f svyazest-agent remove >/dev/null 2>&1 || true
				elif command -v chkconfig >/dev/null 2>&1; then chkconfig svyazest-agent off >/dev/null 2>&1 || true
				fi
			fi ;;
	esac
}

install_docker() {
	darch="$arch"
	case "$(docker version -f '{{.Server.Arch}}' 2>/dev/null)" in
		amd64) darch=amd64 ;;
		arm64) darch=arm64 ;;
		arm) darch=armv7 ;;
	esac
	prog 5
	img="$(download "svyazest-agent-$VERSION-docker-$darch.tar.gz" 5 55)"
	say "Загружаю образ в Docker ..."
	prog 55 80
	gunzip -c "$img" | docker load >/dev/null
	docker tag "svyazest-agent:$VERSION" svyazest-agent:latest
	prog 80 90

	# Служба прежней установки без Docker — не держать два агента.
	disable_native

	mkdir -p "$OPT/data"
	chmod 0700 "$OPT/data"
	cat > "$OPT/docker-compose.yml" <<'YAML'
# «Связь Есть?» — агент в Docker (создано install.sh).
# Меню (вход, состояние): sudo docker exec -it svyazest-agent svyazest-agent
services:
  svyazest-agent:
    image: svyazest-agent:latest
    container_name: svyazest-agent
    network_mode: host
    cap_add:
      - NET_ADMIN
      - NET_RAW
    environment:
      SVYAZEST_DATA: /data
    volumes:
      - ./data:/data
      - /etc/machine-id:/etc/machine-id:ro
      - /run/dbus/system_bus_socket:/run/dbus/system_bus_socket:ro
    restart: unless-stopped
YAML
	if docker compose version >/dev/null 2>&1; then
		(cd "$OPT" && docker compose up -d --force-recreate)
	elif command -v docker-compose >/dev/null 2>&1; then
		(cd "$OPT" && docker-compose up -d --force-recreate)
	else
		# Без compose — то же самое одной командой docker run.
		docker rm -f svyazest-agent >/dev/null 2>&1 || true
		docker run -d --name svyazest-agent --network host \
			--cap-add NET_ADMIN --cap-add NET_RAW \
			-e SVYAZEST_DATA=/data \
			-v "$OPT/data:/data" \
			-v /etc/machine-id:/etc/machine-id:ro \
			-v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket:ro \
			--restart unless-stopped svyazest-agent:latest >/dev/null
	fi
	LOGIN="sudo docker exec -it svyazest-agent svyazest-agent"
}

# ---------------------------------------------------------------- службы
# Тексты служб вставляет build.sh (строки «@FILE …@» → содержимое файла из packaging/).

service_openrc() {
	put /etc/init.d/svyazest-agent 0755 <<'SVYAZEST_EOF'
@FILE openrc/svyazest-agent@
SVYAZEST_EOF
	[ -f /etc/conf.d/svyazest-agent ] || put /etc/conf.d/svyazest-agent 0644 <<'SVYAZEST_EOF'
@FILE openrc/svyazest-agent.confd@
SVYAZEST_EOF
	logrotate_conf
	rc-update add svyazest-agent default >/dev/null 2>&1 || true
	rc-service svyazest-agent restart >/dev/null 2>&1 || rc-service svyazest-agent start \
		|| warn "Не удалось запустить службу OpenRC — запустите: sudo rc-service svyazest-agent start"
	SVC_HINT="sudo rc-service svyazest-agent status   журнал: /var/log/svyazest-agent.log"
}

service_runit() {
	put /etc/sv/svyazest-agent/run 0755 <<'SVYAZEST_EOF'
@FILE runit/run@
SVYAZEST_EOF
	put /etc/sv/svyazest-agent/finish 0755 <<'SVYAZEST_EOF'
@FILE runit/finish@
SVYAZEST_EOF
	put /etc/sv/svyazest-agent/log/run 0755 <<'SVYAZEST_EOF'
@FILE runit/log-run@
SVYAZEST_EOF
	default_conf
	d="$(runit_dir)"
	if [ -e "$d/svyazest-agent" ]; then
		sv restart "$d/svyazest-agent" >/dev/null 2>&1 || true
	else
		ln -s /etc/sv/svyazest-agent "$d/svyazest-agent"
		# runsvdir подхватывает новую службу за ~5 с.
		n=0
		while [ ! -e "$d/svyazest-agent/supervise/ok" ] && [ $n -lt 15 ]; do sleep 1; n=$((n + 1)); done
	fi
	SVC_HINT="sudo sv status svyazest-agent   журнал: /var/log/svyazest-agent/current"
}

service_sysv() {
	put /etc/init.d/svyazest-agent 0755 <<'SVYAZEST_EOF'
@FILE sysv/svyazest-agent@
SVYAZEST_EOF
	default_conf
	logrotate_conf
	if command -v update-rc.d >/dev/null 2>&1; then update-rc.d svyazest-agent defaults >/dev/null 2>&1 || true
	elif command -v chkconfig >/dev/null 2>&1; then chkconfig --add svyazest-agent >/dev/null 2>&1 || true
	elif command -v rc-update >/dev/null 2>&1; then rc-update add svyazest-agent default >/dev/null 2>&1 || true
	else warn "Автозапуск: добавьте «/etc/init.d/svyazest-agent start» в загрузку системы (например, /etc/rc.local)."
	fi
	/etc/init.d/svyazest-agent restart >/dev/null 2>&1 \
		|| warn "Не удалось запустить службу — запустите: sudo /etc/init.d/svyazest-agent start"
	SVC_HINT="sudo /etc/init.d/svyazest-agent status   журнал: /var/log/svyazest-agent.log"
}

service_systemd() { # unit из архива ($1)
	install -m 0644 "$1" /etc/systemd/system/svyazest-agent.service
	systemctl daemon-reload
	systemctl enable svyazest-agent.service >/dev/null 2>&1 || true
	systemctl restart svyazest-agent.service
	SVC_HINT="sudo systemctl status svyazest-agent   журнал: journalctl -u svyazest-agent -f"
}

default_conf() {
	[ -f /etc/default/svyazest-agent ] || put /etc/default/svyazest-agent 0644 <<'SVYAZEST_EOF'
# Дополнительные флаги для «svyazest-agent run», например:
#   SVYAZEST_ARGS="--listen 127.0.0.1:8780"   # веб-страница только на этом компьютере
SVYAZEST_ARGS=""
SVYAZEST_EOF
}

logrotate_conf() {
	[ -d /etc/logrotate.d ] || return 0
	put /etc/logrotate.d/svyazest-agent 0644 <<'SVYAZEST_EOF'
@FILE logrotate/svyazest-agent@
SVYAZEST_EOF
}

install_native() {
	if [ $have_docker = 1 ] && docker inspect svyazest-agent >/dev/null 2>&1; then
		say "Останавливаю контейнер svyazest-agent прежней установки (данные остаются в $OPT/data)."
		docker rm -f svyazest-agent >/dev/null 2>&1 || true
	fi
	SVC_HINT=""
	if command -v dpkg >/dev/null 2>&1 && command -v dpkg-deb >/dev/null 2>&1; then
		# Debian и производные: пакет сам ставит службу systemd или SysV init.
		case "$arch" in armv7) debarch=armhf ;; *) debarch="$arch" ;; esac
		prog 8
		deb="$(download "svyazest-agent_${VERSION}_${debarch}.deb" 8 45)"
		prog 45 70
		if command -v apt-get >/dev/null 2>&1; then
			# Устаревший список пакетов (нужен ca-certificates) — обновить и ещё раз.
			DEBIAN_FRONTEND=noninteractive apt-get install -y "$deb" \
				|| { apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y "$deb"; } \
				|| die "apt-get не смог установить пакет"
		else
			dpkg -i "$deb" || die "dpkg не смог установить пакет"
		fi
		case "$INIT" in
			systemd) SVC_HINT="sudo systemctl status svyazest-agent   журнал: journalctl -u svyazest-agent -f" ;;
			openrc) service_openrc ;;
			runit) service_runit ;;
			sysv) SVC_HINT="sudo /etc/init.d/svyazest-agent status   журнал: /var/log/svyazest-agent.log" ;;
		esac
	else
		prog 8
		tgz="$(download "svyazest-agent-$VERSION-linux-$arch.tar.gz" 8 45)"
		prog 45 70
		tar -xzf "$tgz" -C "$TMP"
		src="$TMP/svyazest-agent-$VERSION-linux-$arch"
		install -d -m 0755 /usr/lib/svyazest-agent
		for f in svyazest-agent xray xray-next; do
			install -m 0755 "$src/$f" "/usr/lib/svyazest-agent/$f"
		done
		ln -sf /usr/lib/svyazest-agent/svyazest-agent /usr/bin/svyazest-agent
		install -d -m 0700 /var/lib/svyazest-agent
		case "$INIT" in
			systemd) service_systemd "$src/svyazest-agent.service" ;;
			openrc) service_openrc ;;
			runit) service_runit ;;
			sysv) service_sysv ;;
			*)
				warn "Система запуска служб не найдена: запускайте агент сами, например:"
				warn "  SVYAZEST_SERVICE=1 /usr/lib/svyazest-agent/svyazest-agent run --data-dir /var/lib/svyazest-agent" ;;
		esac
	fi
	LOGIN="sudo svyazest-agent"
}

# ---------------------------------------------------------------- ModemManager
# Нужен для обычных USB-модемов (оператор, сигнал, DNS модема). HiLink и
# телефон в режиме USB-модема работают и без него.
mm_installed() { command -v mmcli >/dev/null 2>&1 || command -v ModemManager >/dev/null 2>&1; }

mm_hint() {
	if command -v apt-get >/dev/null 2>&1; then echo "sudo apt install modemmanager"
	elif command -v dnf >/dev/null 2>&1; then echo "sudo dnf install ModemManager && sudo systemctl enable --now ModemManager"
	elif command -v yum >/dev/null 2>&1; then echo "sudo yum install ModemManager && sudo systemctl enable --now ModemManager"
	elif command -v pacman >/dev/null 2>&1; then echo "sudo pacman -S modemmanager && sudo systemctl enable --now ModemManager"
	elif command -v zypper >/dev/null 2>&1; then echo "sudo zypper install ModemManager && sudo systemctl enable --now ModemManager"
	elif command -v apk >/dev/null 2>&1; then echo "sudo apk add modemmanager dbus && sudo rc-update add dbus && sudo rc-update add modemmanager && sudo rc-service modemmanager start"
	elif command -v xbps-install >/dev/null 2>&1; then echo "sudo xbps-install -S ModemManager dbus && sudo ln -s /etc/sv/dbus /etc/sv/modemmanager /var/service/"
	elif command -v emerge >/dev/null 2>&1; then echo "sudo emerge net-misc/modemmanager"
	else echo "поставьте пакет ModemManager средствами вашей системы"
	fi
}

mm_enable() { # служба ModemManager (и D-Bus) — включить и запустить
	case "$INIT" in
		systemd) systemctl enable --now ModemManager.service >/dev/null 2>&1 || true ;;
		openrc)
			for s in dbus modemmanager; do
				[ -f "/etc/init.d/$s" ] || continue
				rc-update add "$s" default >/dev/null 2>&1 || true
				rc-service "$s" start >/dev/null 2>&1 || true
			done ;;
		runit)
			d="$(runit_dir)"
			# В пакете Void у ModemManager нет службы runit — своя, после D-Bus.
			if [ ! -d /etc/sv/modemmanager ] && [ ! -d /etc/sv/ModemManager ] && command -v ModemManager >/dev/null 2>&1; then
				put /etc/sv/modemmanager/run 0755 <<'SVYAZEST_EOF'
#!/bin/sh
# ModemManager для «Связь Есть?» (создано install.sh): запускается после D-Bus.
sv check dbus >/dev/null 2>&1 || exit 1
exec ModemManager 2>&1
SVYAZEST_EOF
			fi
			for s in dbus modemmanager ModemManager; do
				[ -d "/etc/sv/$s" ] && [ ! -e "$d/$s" ] && ln -s "/etc/sv/$s" "$d/$s"
			done ;;
		sysv)
			for s in dbus modemmanager; do
				[ -x "/etc/init.d/$s" ] && "/etc/init.d/$s" start >/dev/null 2>&1 || true
			done ;;
	esac
	return 0
}

# pacman без базы пакетов (свежая установка) не находит пакет — тогда -Sy.
pacman_get() {
	pacman -S --noconfirm --needed "$@" >/dev/null 2>&1 || pacman -Sy --noconfirm --needed "$@" >/dev/null 2>&1
}

install_modemmanager() {
	say "Ставлю ModemManager (для USB-модемов) ..."
	ok=0
	if command -v apt-get >/dev/null 2>&1; then
		if DEBIAN_FRONTEND=noninteractive apt-get install -y modemmanager \
			|| { apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y modemmanager; }; then ok=1; fi
	elif command -v dnf >/dev/null 2>&1; then dnf install -y ModemManager >/dev/null 2>&1 && ok=1
	elif command -v yum >/dev/null 2>&1; then yum install -y ModemManager >/dev/null 2>&1 && ok=1
	elif command -v pacman >/dev/null 2>&1; then pacman_get modemmanager && ok=1
	elif command -v zypper >/dev/null 2>&1; then zypper --non-interactive install ModemManager >/dev/null 2>&1 && ok=1
	elif command -v apk >/dev/null 2>&1; then apk add --no-progress modemmanager dbus >/dev/null 2>&1 && ok=1
	elif command -v xbps-install >/dev/null 2>&1; then xbps-install -Sy ModemManager dbus >/dev/null 2>&1 && ok=1
	fi
	if [ $ok = 1 ]; then
		mm_enable
		say "ModemManager установлен."
	else
		warn "ModemManager поставить не получилось. Для обычного USB-модема поставьте его сами:"
		warn "  $(mm_hint)"
		warn "Модемы HiLink и телефон в режиме USB-модема работают и без него."
	fi
}

# Сертификаты для HTTPS (агент — статическая программа и берёт их из системы).
have_ca() {
	for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/ca-bundle.pem /etc/ssl/cert.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem; do
		[ -s "$f" ] && return 0
	done
	return 1
}
ensure_ca() {
	have_ca && return 0
	say "Ставлю сертификаты (ca-certificates) ..."
	if command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates >/dev/null 2>&1
	elif command -v dnf >/dev/null 2>&1; then dnf install -y ca-certificates >/dev/null 2>&1
	elif command -v yum >/dev/null 2>&1; then yum install -y ca-certificates >/dev/null 2>&1
	elif command -v pacman >/dev/null 2>&1; then pacman_get ca-certificates
	elif command -v zypper >/dev/null 2>&1; then zypper --non-interactive install ca-certificates >/dev/null 2>&1
	elif command -v apk >/dev/null 2>&1; then apk add --no-progress ca-certificates >/dev/null 2>&1
	elif command -v xbps-install >/dev/null 2>&1; then xbps-install -Sy ca-certificates >/dev/null 2>&1
	fi
	have_ca || warn "Внимание: не найдены сертификаты HTTPS — поставьте пакет ca-certificates."
	return 0
}

case "$INIT" in
	systemd) INIT_NAME=systemd ;; openrc) INIT_NAME=OpenRC ;; runit) INIT_NAME=runit ;;
	sysv) INIT_NAME="SysV init" ;; *) INIT_NAME="без системы запуска" ;;
esac
say "«Связь Есть?» $VERSION: архитектура $arch, установка $( [ "$MODE" = docker ] && echo "в Docker" || echo "без Docker (служба: $INIT_NAME)" )."
if [ "$MODE" = docker ]; then
	install_docker
else
	prog 1 8
	ensure_ca
	install_native
fi

# ModemManager нужен на самой системе и для Docker (агент читает его по D-Bus).
if ! mm_installed; then
	if [ $WITH_MM = 1 ]; then
		if [ "$MODE" = docker ]; then prog 90 99; else prog 70 99; fi
		install_modemmanager
	else
		warn "ModemManager не стоит. Для обычного USB-модема: $(mm_hint)"
	fi
fi

ADDR="$(ip_addr || true)"
[ -n "$ADDR" ] || ADDR="<адрес-этого-компьютера>"
prog 100
[ "$P_ON" = 1 ] && printf '\n' >&3
if [ -s "$TMP/warn" ]; then
	out ""
	cat "$TMP/warn" >&3
fi
out ""
out "Готово: «Связь Есть?» $VERSION установлен. Осталось войти в аккаунт партнёра —"
out "откройте меню агента (вход, состояние, служба, журнал):"
out "  $LOGIN"
out "Веб-страница агента: http://$ADDR:8780"
if [ "$P_ON" = 1 ]; then
	say "Служба: ${SVC_HINT:-}"
else
	[ -z "${SVC_HINT:-}" ] || say "Служба: $SVC_HINT"
	say "Обновление — та же команда установки ещё раз."
fi
