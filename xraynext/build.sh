#!/bin/bash
# Builds android/app/src/main/jniLibs/arm64-v8a/libxraynext.so -- Xray-core
# v26.10.31 as a separate executable (XrayNext.kt), next to the embedded
# 26.3.27 (xraylib). Upstream source + zz_svyazest_android.go (pins the
# process to the cellular network). Needs Go 1.27 and the Android NDK.
#
#   NDK=/root/build/android-sdk/ndk/28.2.13676358 bash xraynext/build.sh
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/../android/app/src/main/jniLibs/arm64-v8a/libxraynext.so}"
TAG=v26.10.31   # commit a4a3dff5b20c2faae567b742c172c7b95e75c083
: "${NDK:?set NDK to the Android NDK directory}"
SRC="$(mktemp -d)"
git clone -q --depth 1 --branch "$TAG" https://github.com/XTLS/Xray-core "$SRC"
cp "$HERE/zz_svyazest_android.go" "$SRC/main/"
cd "$SRC"
# -checklinkname=0: github.com/wlynxg/anet links to net internals (Android);
# upstream's own Android builds use the same flag.
CC="$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android24-clang" \
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  go build -trimpath -ldflags "-checklinkname=0 -s -w -buildid=" -o "$OUT" ./main
ls -la "$OUT"
