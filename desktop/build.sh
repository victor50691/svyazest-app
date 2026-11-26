#!/bin/bash
# «Связь Есть?» для компьютеров — полная сборка выпуска.
# Запускается из папки проекта (desktop/ в репозитории):
#
#   ./build.sh                                        # всё
#   SKIP_DOCKER=1 ./build.sh                          # без образов Docker
#   VERSION=1.0.1 ./build.sh
#
# Собирает svyazest-agent и xray-core двух версий под linux/amd64,
# linux/arm64, linux/arm (GOARM=7) и windows/amd64:
#   xray      = 26.3.27  (d2758a0) — та же, что встроена в Android (xraylib)
#   xray-next = 26.10.31 (a4a3dff)
# Исходники Xray-core обеих версий клонируются с github.com/XTLS/Xray-core
# в кэш (XRAY_CACHE, по умолчанию ~/.cache/svyazest-build).
# и складывает в dist/: архивы .tar.gz, .deb, установщик Windows (NSIS) + zip,
# образы Docker (docker save | gzip), install.sh, docker-compose.yml, SHA256SUMS.
#
# Нужно: Go (/usr/local/go/bin), dpkg-deb, makensis (nsis), zip, docker + buildx
# с qemu-user-binfmt для чужих архитектур.
set -euo pipefail

VERSION="${VERSION:-1.0.10}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
DIST="$ROOT/dist"
WORK="$DIST/.work"
CACHE="${XRAY_CACHE:-$HOME/.cache/svyazest-build}"
XRAY_TAG=v26.3.27
XRAY_COMMIT=d2758a023cd7f4174a5a5fa4ff66e487d4342ba0
XRAY_SRC="$CACHE/xray-$XRAY_TAG"
XRAY_NEXT_TAG=v26.10.31
XRAY_NEXT_SRC="${XRAY_NEXT_SRC:-$CACHE/xray-$XRAY_NEXT_TAG}"
XRAY_NEXT_COMMIT=a4a3dff5b20c2faae567b742c172c7b95e75c083
export PATH="/usr/local/go/bin:$PATH"
export CGO_ENABLED=0
# Сборка не должна ничего дописывать в исходники xray (go.mod/go.sum).
export GOFLAGS=-mod=readonly

# name GOOS GOARCH GOARM debarch docker-platform
TARGETS=(
	"amd64 linux amd64 '' amd64 linux/amd64"
	"arm64 linux arm64 '' arm64 linux/arm64"
	"armv7 linux arm 7 armhf linux/arm/v7"
	"windows-amd64 windows amd64 '' - -"
)

log() { printf '\n== %s\n' "$*"; }
die() { printf 'ОШИБКА: %s\n' "$*" >&2; exit 1; }

for t in go dpkg-deb tar gzip sha256sum; do command -v $t >/dev/null || die "нет $t"; done

rm -rf "$DIST"
mkdir -p "$DIST" "$WORK" "$CACHE"

# ---------------------------------------------------------------- исходники xray
if [ ! -d "$XRAY_SRC/.git" ]; then
	log "клонирую Xray-core $XRAY_TAG в $XRAY_SRC"
	git clone -q --depth 1 --branch "$XRAY_TAG" https://github.com/XTLS/Xray-core "$XRAY_SRC"
fi
[ "$(git -C "$XRAY_SRC" rev-parse HEAD)" = "$XRAY_COMMIT" ] || die "$XRAY_SRC не на коммите $XRAY_COMMIT"
if [ ! -d "$XRAY_NEXT_SRC/.git" ]; then
	log "клонирую Xray-core $XRAY_NEXT_TAG в $XRAY_NEXT_SRC"
	git clone -q --depth 1 --branch "$XRAY_NEXT_TAG" https://github.com/XTLS/Xray-core "$XRAY_NEXT_SRC"
fi
[ "$(git -C "$XRAY_NEXT_SRC" rev-parse HEAD)" = "$XRAY_NEXT_COMMIT" ] || die "$XRAY_NEXT_SRC не на коммите $XRAY_NEXT_COMMIT"

# xray собирается долго и не меняется — кэш по версии и цели.
build_xray() { # src ver goos goarch goarm out
	local src=$1 ver=$2 goos=$3 goarch=$4 goarm=$5 out=$6
	# Коммит в «xray version» задаём явно: в исходниках может лежать
	# неотслеживаемый файл (zz_svyazest_android.go), и по VCS вышло бы «-dirty».
	local commit; commit="$(git -C "$src" rev-parse --short=7 HEAD)"
	local ext=""; [ "$goos" = windows ] && ext=.exe
	local cached="$CACHE/bin/xray-$ver-$commit/$goos-$goarch${goarm:+v$goarm}/xray$ext"
	if [ ! -x "$cached" ] || [ -n "${FORCE_XRAY:-}" ]; then
		mkdir -p "$(dirname "$cached")"
		echo "   xray $ver $goos/$goarch${goarm:+ v$goarm}"
		(cd "$src" && GOOS=$goos GOARCH=$goarch GOARM=$goarm \
			go build -trimpath -buildvcs=false \
			-ldflags "-s -w -buildid= -X github.com/xtls/xray-core/core.build=$commit" -o "$cached.tmp" ./main)
		mv "$cached.tmp" "$cached"
	fi
	cp "$cached" "$out"
}

# ---------------------------------------------------------------- бинарники
for t in "${TARGETS[@]}"; do
	eval "set -- $t"
	name=$1 goos=$2 goarch=$3 goarm=$4
	ext=""; [ "$goos" = windows ] && ext=.exe
	out="$WORK/bin/$name"
	mkdir -p "$out"
	log "сборка $goos/$goarch${goarm:+ v$goarm}"
	(cd "$ROOT" && GOOS=$goos GOARCH=$goarch GOARM=$goarm \
		go build -trimpath -ldflags "-s -w -buildid= -X main.version=$VERSION" \
		-o "$out/svyazest-agent$ext" ./cmd/svyazest-agent)
	build_xray "$XRAY_SRC" 26.3.27 "$goos" "$goarch" "$goarm" "$out/xray$ext"
	build_xray "$XRAY_NEXT_SRC" 26.10.31 "$goos" "$goarch" "$goarm" "$out/xray-next$ext"
done

# Тексты с подстановкой версии/архитектуры.
subst() { sed -e "s/@VERSION@/$VERSION/g" -e "s/@ARCH@/${2:-}/g" "$1"; }

# ---------------------------------------------------------------- Linux: tar.gz + .deb
for t in "${TARGETS[@]}"; do
	eval "set -- $t"
	name=$1 goos=$2 debarch=$5
	[ "$goos" = linux ] || continue
	base="svyazest-agent-$VERSION-linux-$name"
	log "архив и .deb для $name"
	st="$WORK/tar/$base"
	mkdir -p "$st"
	install -m 0755 "$WORK/bin/$name/svyazest-agent" "$WORK/bin/$name/xray" "$WORK/bin/$name/xray-next" "$st/"
	install -m 0644 "$ROOT/packaging/systemd/svyazest-agent.service" "$st/"
	# 1.0.2: службы для систем без systemd (их же ставит install.sh).
	install -d "$st/openrc" "$st/runit/log" "$st/sysv" "$st/logrotate"
	install -m 0755 "$ROOT/packaging/openrc/svyazest-agent" "$st/openrc/svyazest-agent"
	install -m 0644 "$ROOT/packaging/openrc/svyazest-agent.confd" "$st/openrc/svyazest-agent.confd"
	install -m 0755 "$ROOT/packaging/runit/run" "$ROOT/packaging/runit/finish" "$st/runit/"
	install -m 0755 "$ROOT/packaging/runit/log-run" "$st/runit/log/run"
	install -m 0755 "$ROOT/packaging/sysv/svyazest-agent" "$st/sysv/svyazest-agent"
	install -m 0644 "$ROOT/packaging/logrotate/svyazest-agent" "$st/logrotate/svyazest-agent"
	subst "$ROOT/packaging/README.txt" "$name" > "$st/README.txt"
	# Каталог верхнего уровня по имени, без записи «./» (см. владельца при распаковке от root).
	tar -C "$WORK/tar" --owner=0 --group=0 --numeric-owner -czf "$DIST/$base.tar.gz" "$base"

	r="$WORK/deb/$name"
	install -d "$r/DEBIAN" "$r/usr/lib/svyazest-agent" "$r/usr/bin" "$r/usr/lib/systemd/system" \
		"$r/etc/default" "$r/etc/init.d" "$r/usr/share/doc/svyazest-agent"
	install -m 0755 "$WORK/bin/$name/svyazest-agent" "$WORK/bin/$name/xray" "$WORK/bin/$name/xray-next" "$r/usr/lib/svyazest-agent/"
	ln -sf ../lib/svyazest-agent/svyazest-agent "$r/usr/bin/svyazest-agent"
	install -m 0644 "$ROOT/packaging/systemd/svyazest-agent.service" "$r/usr/lib/systemd/system/"
	install -m 0644 "$ROOT/packaging/deb/default" "$r/etc/default/svyazest-agent"
	# 1.0.2: SysV init для Debian без systemd (Devuan); systemd его не трогает.
	install -m 0755 "$ROOT/packaging/sysv/svyazest-agent" "$r/etc/init.d/svyazest-agent"
	subst "$ROOT/packaging/README.txt" "$name" > "$r/usr/share/doc/svyazest-agent/README.txt"
	for s in postinst prerm postrm; do install -m 0755 "$ROOT/packaging/deb/$s" "$r/DEBIAN/$s"; done
	printf '%s\n' /etc/default/svyazest-agent /etc/init.d/svyazest-agent > "$r/DEBIAN/conffiles"
	size=$(du -sk --exclude=DEBIAN "$r" | cut -f1)
	sed -e "s/@VERSION@/$VERSION/" -e "s/@DEBARCH@/$debarch/" -e "s/@SIZE@/$size/" \
		"$ROOT/packaging/deb/control.in" > "$r/DEBIAN/control"
	dpkg-deb --root-owner-group -Zxz --build "$r" "$DIST/svyazest-agent_${VERSION}_${debarch}.deb" >/dev/null
done

# ---------------------------------------------------------------- Windows: zip + NSIS
log "Windows: zip и установщик"
wbase="svyazest-agent-$VERSION-windows-amd64"
wst="$WORK/win/$wbase"
mkdir -p "$wst"
cp "$WORK/bin/windows-amd64/"*.exe "$wst/"
# README в Windows — с переводами строк CRLF (Блокнот старых версий).
subst "$ROOT/packaging/windows/README.txt" | sed 's/$/\r/' > "$wst/README.txt"
if command -v zip >/dev/null; then
	(cd "$WORK/win" && zip -qr -X "$DIST/$wbase.zip" "$wbase")
else
	echo "   нет zip — архив Windows пропущен" >&2
fi
if command -v makensis >/dev/null; then
	makensis -V2 -DVERSION="$VERSION" -DSRCDIR="$wst" -DOUTFILE="$DIST/$wbase-setup.exe" \
		"$ROOT/packaging/windows/installer.nsi"
	# Постоянная ссылка из бота — без версии (копия, не симлинк: файлы
	# выкладываются в релиз GitHub по одному).
	cp "$DIST/$wbase-setup.exe" "$DIST/svyazest-agent-windows-setup.exe"
else
	die "нет makensis (apt install nsis)"
fi

# ---------------------------------------------------------------- Docker
if [ -z "${SKIP_DOCKER:-}" ]; then
	command -v docker >/dev/null || die "нет docker (или SKIP_DOCKER=1)"
	ctx="$WORK/docker"
	mkdir -p "$ctx/bin"
	cp "$ROOT/packaging/docker/Dockerfile" "$ctx/"
	for t in "${TARGETS[@]}"; do
		eval "set -- $t"
		name=$1 goos=$2 plat=$6
		[ "$goos" = linux ] || continue
		# TARGETARCH+TARGETVARIANT: amd64, arm64, armv7
		mkdir -p "$ctx/bin/$name"
		cp "$WORK/bin/$name/svyazest-agent" "$WORK/bin/$name/xray" "$WORK/bin/$name/xray-next" "$ctx/bin/$name/"
	done
	for t in "${TARGETS[@]}"; do
		eval "set -- $t"
		name=$1 goos=$2 plat=$6
		[ "$goos" = linux ] || continue
		log "образ Docker $plat"
		docker buildx build --quiet --platform "$plat" --build-arg VERSION="$VERSION" \
			-t "svyazest-agent:$VERSION" --load "$ctx" >/dev/null
		docker save "svyazest-agent:$VERSION" | gzip -9 > "$DIST/svyazest-agent-$VERSION-docker-$name.tar.gz"
		docker image rm "svyazest-agent:$VERSION" >/dev/null
	done
fi

# ---------------------------------------------------------------- раздача
cp "$ROOT/packaging/docker/docker-compose.yml" "$DIST/docker-compose.yml"
# Строки «@FILE путь@» в install.sh — содержимое файлов из packaging/ (службы
# OpenRC / runit / SysV init и logrotate, 1.0.2): один источник текстов.
subst "$ROOT/packaging/install.sh" | awk -v dir="$ROOT/packaging" '
	/^@FILE .*@$/ {
		f = substr($2, 1, length($2) - 1)
		n = 0
		while ((getline line < (dir "/" f)) > 0) { print line; n++ }
		close(dir "/" f)
		if (n == 0) { print "build.sh: пустой или нет файла " f > "/dev/stderr"; exit 1 }
		next
	}
	{ print }' > "$DIST/install.sh"
grep -q '^@FILE' "$DIST/install.sh" && die "install.sh: не все @FILE подставлены"
chmod 0755 "$DIST/install.sh"
(cd "$DIST" && sha256sum -- *.tar.gz *.deb *.zip *.exe install.sh docker-compose.yml 2>/dev/null > SHA256SUMS || true)
rm -rf "$WORK"

log "готово: $DIST"
ls -la "$DIST"
