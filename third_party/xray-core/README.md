# Xray-core (embedded)

Since app version 1.3.0 [XTLS/Xray-core](https://github.com/XTLS/Xray-core)
(release `v26.3.27`, commit `d2758a023cd7f4174a5a5fa4ff66e487d4342ba0`) is
compiled INTO the app as a gomobile library, `android/app/libs/xraylib.aar`,
built from `../../xraylib/` (see `xraylib/build.sh`). The old approach — the
official `xray` executable shipped as `jniLibs/arm64-v8a/libxray.so` and
spawned as a child process — is gone.

Why: a child process cannot be pinned to the phone's cellular network, so
with a third-party VPN switched on its traffic went through that VPN and
the check result was about the VPN, not the mobile connection. Inside the
app process every socket Xray opens passes through
`internet.RegisterDialerController` (a public, `xray:api:beta` hook) and is
bound with `android.net.Network.bindSocket()` to the cellular network before
it connects — see `NativePlugin.kt`'s `socketBinder`.

`geoip.dat` / `geosite.dat` are still not bundled: the generated config
never routes by geo rules.

Only `arm64-v8a` is built (gomobile `-target android/arm64`). On a 32-bit
device VPN-key checks report an unsupported architecture; IP/domain checks
are pure Dart and unaffected.

License: Xray-core is MPL-2.0, see `LICENSE` in this directory.
