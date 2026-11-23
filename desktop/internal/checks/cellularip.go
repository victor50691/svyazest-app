package checks

// Port of lib/services/cellular_ip.dart: the operator's public IP, asked
// over the modem right before a job. First our own /executor/v1/ip, then
// (under «белые списки» it is usually unreachable) Yandex's echo.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/netbind"
)

// AppVersion goes out as X-App-Version with the /ip request (the Android
// app sends its pubspec version). The app package may overwrite it.
var AppVersion = "1.0.1"

const yandexIPURL = "https://ipv4-internet.yandex.net/api/v0/ip"

var cellIPCache struct {
	sync.Mutex
	at   time.Time
	last map[string]any
}

func ipEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		base = "https://dpichecker.st"
	}
	if strings.HasSuffix(base, "/executor/v1") {
		return base + "/ip"
	}
	return base + "/executor/v1/ip"
}

func cellularIPImpl(ctx context.Context, env Env) map[string]any {
	cellIPCache.Lock()
	if cellIPCache.last != nil && cellIPCache.last["ip"] != nil && time.Since(cellIPCache.at) < 2*time.Minute {
		out := cellIPCache.last
		cellIPCache.Unlock()
		return copyMap(out)
	}
	cellIPCache.Unlock()

	var errs []string
	var ip, via any
	if env.Net == nil {
		return map[string]any{"ip": nil, "via": nil, "error": "сервер: no cellular network"}
	}
	h := map[string]string{"X-App-Version": AppVersion}
	if env.DeviceToken != "" {
		h["Authorization"] = "Bearer " + env.DeviceToken
	}
	if body, err := cellGet(ctx, env, ipEndpoint(env.APIBase), h); err != nil {
		errs = append(errs, "сервер: "+short80(err.Error()))
	} else {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			errs = append(errs, "сервер: "+short80("FormatException: "+err.Error()))
		} else if v, ok := m["ip"].(string); ok && v != "" {
			ip, via = v, "dpichecker"
		}
	}
	if ip == nil {
		body, err := cellGet(ctx, env, yandexIPURL, map[string]string{
			"User-Agent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36",
			"Origin":     "https://yandex.ru",
			"Referer":    "https://yandex.ru/internet/",
		})
		if err != nil {
			errs = append(errs, "яндекс: "+short80(err.Error()))
		} else {
			var v any
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				errs = append(errs, "яндекс: "+short80("FormatException: "+err.Error()))
			} else if s, ok := v.(string); ok && s != "" && s != "127.0.0.1" {
				ip, via = s, "yandex"
			} else {
				errs = append(errs, "яндекс: ответ "+body)
			}
		}
	}
	out := map[string]any{"ip": ip, "via": via}
	if ip == nil {
		out["error"] = strings.Join(errs, "; ")
	}
	cellIPCache.Lock()
	cellIPCache.at, cellIPCache.last = time.Now(), out
	cellIPCache.Unlock()
	return copyMap(out)
}

// cellGet: connect 5 s, request 6 s, body 6 s (the Dart timeouts).
func cellGet(ctx context.Context, env Env, url string, headers map[string]string) (string, error) {
	return cellGetFamily(ctx, env, 0, url, headers, 5*time.Second, 6*time.Second)
}

// cellGetFamily: GET over the modem, IP family 4 / 6 only (0 = any);
// connect within connect, the rest (TLS, headers, body) within step each.
func cellGetFamily(ctx context.Context, env Env, family int, url string, headers map[string]string, connect, step time.Duration) (string, error) {
	tr := binderTransportFamily(env, family, connect, step, step)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", dartIOError(err, step)
	}
	defer resp.Body.Close()
	t := time.AfterFunc(step, cancel)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	expired := !t.Stop()
	if err != nil {
		if expired {
			return "", errors.New("TimeoutException after " + fmtDartDuration(step) + ": Future not completed")
		}
		return "", dartIOError(err, step)
	}
	if resp.StatusCode != 200 {
		return "", &httpStatusErr{resp.StatusCode}
	}
	return strings.TrimSpace(string(b)), nil
}

// dartIOError: a Dart-looking exception text (no URL in it).
func dartIOError(err error, budget time.Duration) error {
	switch dartErrType(err) {
	case "TimeoutException":
		return errors.New("TimeoutException after " + fmtDartDuration(budget) + ": Future not completed")
	case "HandshakeException":
		return errors.New("HandshakeException: " + shortErr(err))
	case "HttpException":
		return errors.New("HttpException: " + shortErr(err))
	}
	return errors.New("SocketException: " + shortErr(err))
}

func short80(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:80])
	}
	return s
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// httpStatusErr: a non-200 answer (Dart's text "HttpException: HTTP 403").
type httpStatusErr struct{ code int }

func (e *httpStatusErr) Error() string { return "HttpException: HTTP " + strconv.Itoa(e.code) }

// Yandex answers the real address only to browser-like requests.
var yandexEchoHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36",
	"Origin":     "https://yandex.ru",
	"Referer":    "https://yandex.ru/internet/",
}

// cellularEchoImpl = ApiClient.cellularEcho(family): GET
// https://ipv{4,6}-internet.yandex.net/api/v0/ip over the modem, pinned to
// that family (connect 8 s, read 8 s as NativePlugin.cellularGet).
func cellularEchoImpl(ctx context.Context, env Env, family int) (string, string) {
	if family != 6 {
		family = 4
	}
	if env.Net == nil {
		return "", "no cellular network"
	}
	url := "https://ipv" + strconv.Itoa(family) + "-internet.yandex.net/api/v0/ip"
	body, err := cellGetFamily(ctx, env, family, url, yandexEchoHeaders, 8*time.Second, 8*time.Second)
	// Yandex's balancer now and then answers 504 (seen on the build server
	// in up to 1 of 5 IPv6 requests): two more tries, or a 10-minute IPv6
	// measurement would be lost to it.
	for try := 1; try <= 2; try++ {
		var he0 *httpStatusErr
		if !errors.As(err, &he0) || he0.code < 500 {
			break
		}
		select {
		case <-ctx.Done():
			return "", "HTTP " + strconv.Itoa(he0.code)
		case <-time.After(time.Duration(try) * time.Second):
		}
		body, err = cellGetFamily(ctx, env, family, url, yandexEchoHeaders, 8*time.Second, 8*time.Second)
	}
	if err != nil {
		var he *httpStatusErr
		if errors.As(err, &he) {
			return "", "HTTP " + strconv.Itoa(he.code)
		}
		return "", short80(err.Error())
	}
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		if s, ok := v.(string); ok && s != "" && s != "127.0.0.1" && (family == 4) != strings.Contains(s, ":") {
			return s, ""
		}
	}
	b := []rune(strings.TrimSpace(body))
	if len(b) > 60 {
		b = b[:60]
	}
	return "", "ответ " + string(b)
}

// modemHasGlobal6 = NativePlugin.cellularIpv6().global: the modem has a
// global IPv6 address (not link-local, not unique-local fc00::/7) right
// now. No traffic -- cheap enough before every IPv6 job.
func modemHasGlobal6(env Env) bool {
	if env.Net == nil {
		return false
	}
	ifc := env.Net.Iface()
	addrs := ifc.Addrs
	if list, err := netbind.List(); err == nil {
		for _, i := range list {
			if i.Name == ifc.Name {
				addrs = i.Addrs
				break
			}
		}
	}
	for _, a := range addrs {
		if a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsPrivate() && !isNat64(a) {
			return true
		}
	}
	return false
}

// ipv6ProbeImpl = _measureIpv6.
func ipv6ProbeImpl(ctx context.Context, env Env) (bool, map[string]any) {
	out := map[string]any{"global": false, "ip": nil, "error": nil}
	if env.Net == nil {
		out["error"], out["why"] = "no cellular network", "нет IPv6 в мобильной сети (модема нет)"
		return false, out
	}
	if !modemHasGlobal6(env) {
		out["error"], out["why"] = "у модема нет глобального IPv6-адреса", "нет IPv6 в мобильной сети (адреса нет)"
		return false, out
	}
	out["global"] = true
	ip, errText := cellularEchoImpl(ctx, env, 6)
	if ip == "" {
		out["error"], out["why"] = errText, "IPv6-адрес есть, но эхо не ответило: "+errText
		return false, out
	}
	out["ip"], out["why"] = ip, "IPv6 работает"
	return true, out
}
