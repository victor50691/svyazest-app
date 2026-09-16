#!/bin/bash
# Builds android/app/libs/xraylib.aar -- Xray-core wrapped as a gomobile
# library (see ../third_party/xray-core/README.md for why it lives inside
# the app process). Needs Go and an Android SDK with an NDK installed;
# normal app builds don't need this, the prebuilt .aar is in the repo.
#
#   ANDROID_HOME=~/Android/Sdk ANDROID_NDK_HOME=~/Android/Sdk/ndk/<ver> \
#     bash xraylib/build.sh
#
# Takes ~10-15 minutes and a few GB of RAM on a first run.
set -e
trap 'echo GOMOBILE_FAILED' ERR

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/../android/app/libs/xraylib.aar}"
GO_VERSION="${GO_VERSION:-1.27.1}"
XRAY_COMMIT=d2758a023cd7f4174a5a5fa4ff66e487d4342ba0   # tag v26.3.27

: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
: "${ANDROID_NDK_HOME:?set ANDROID_NDK_HOME to the NDK inside that SDK}"
export ANDROID_HOME ANDROID_NDK_HOME
export PATH="/usr/local/go/bin:$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin:$PATH"

if ! command -v go >/dev/null; then
  curl -sL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | sudo tar -C /usr/local -xz
fi
go version
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init

cd "$HERE"
go get github.com/xtls/xray-core@$XRAY_COMMIT
go mod tidy
# Go 1.24+: gomobile needs x/mobile in the module graph; a tool directive
# survives `go mod tidy`, a plain `go get` of the bind package does not.
go get -tool golang.org/x/mobile/cmd/gobind@latest
go get golang.org/x/mobile/bind@latest
gomobile bind -v -target android/arm64 -androidapi 24 -ldflags "-s -w" -o "$OUT" .
ls -la "$OUT"
echo GOMOBILE_DONE
