package checks

// Port of lib/services/checkers/ip_checker.dart: plain IP / domain
// reachability straight from the modem, no xray.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	minPayloadBytes    = 49152
	maxPayloadBytes    = 800000
	stallMaxBytes      = 40000
	maxAssetCandidates = 6
	maxRedirects       = 10
)

var literalV4Re = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)

// checkIPOrDomain = checkIpOrDomain (Android 1.17.2). ipFamily (the job's
// ip_family): 6 = the domain's AAAA from the modem's DNS and every
// connection over IPv6 only, 4 = over IPv4 only, 0 = as before. A literal
// address is its own family.
func checkIPOrDomain(ctx context.Context, env Env, target string, ipFamily int) map[string]any {
	if literalV4Re.MatchString(target) {
		return checkIP(ctx, env, target, 4)
	}
	if v6, ok := asLiteralIPv6(target); ok {
		return checkIP(ctx, env, v6, 6)
	}
	if ipFamily != 4 && ipFamily != 6 {
		ipFamily = 0
	}
	return checkDomain(ctx, env, target, ipFamily)
}

// asLiteralIPv6 = the IPv6 half of _asLiteralIp: bare or in brackets.
func asLiteralIPv6(s string) (string, bool) {
	bare := s
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		bare = s[1 : len(s)-1]
	}
	if !strings.Contains(bare, ":") || strings.Contains(bare, "%") {
		return "", false
	}
	a, err := netip.ParseAddr(bare)
	if err != nil || !a.Is6() {
		return "", false
	}
	return bare, true
}

// isBogusIP6 = _isBogusIp6: an AAAA that cannot be the site (unspecified,
// loopback, link-local, multicast, unique-local, IPv4-mapped).
func isBogusIP6(a netip.Addr) bool {
	if !a.Is6() || a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.Is4In6() {
		return true
	}
	b := a.As16()
	return b[0]&0xfe == 0xfc
}

func famOrNil(f int) any {
	if f == 4 || f == 6 {
		return f
	}
	return nil
}

// ipResult = IpCheckResult.toJson: every key always present.
func ipResult() map[string]any {
	m := map[string]any{}
	for _, k := range []string{"accessible", "resolved_ip", "tcp_latency_ms", "port_80_open", "port_443_open", "tls_ok",
		"error", "http_code", "bytes", "final_host", "final_scheme", "block_reason", "time_ms", "payload_ok",
		"payload_bytes", "payload_url", "payload_from", "stall_bytes", "anti_bot", "redirects", "redirect_limit", "ip_family"} {
		m[k] = nil
	}
	m["accessible"] = false
	return m
}

func isBogusIP(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return true
	}
	p := make([]int, 4)
	for i, s := range parts {
		n, ok := dartTryParseInt(s)
		if !ok {
			return true
		}
		p[i] = n
	}
	a, b := p[0], p[1]
	return a == 0 || a == 127 || a == 10 || (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168) ||
		(a == 100 && b >= 64 && b <= 127) || (a == 169 && b == 254) || a >= 224
}

func registrable(host string) string {
	h := strings.ToLower(host)
	parts := strings.Split(h, ".")
	if len(parts) <= 2 {
		return h
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func httpsRoot(domain string) *url.URL { return &url.URL{Scheme: "https", Host: domain, Path: "/"} }

// parseRef = Uri.parse, tolerating what Dart would have percent-encoded.
func parseRef(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		u, err = url.Parse(strings.ReplaceAll(s, " ", "%20"))
	}
	return u, err
}

// walkRedirects: a Location from a plain-http page to another domain may
// have been injected by the operator.
func walkRedirects(domain string, locations []*url.URL) (*url.URL, bool) {
	at := httpsRoot(domain)
	untrusted := false
	for _, loc := range locations {
		to := at.ResolveReference(loc)
		if at.Scheme != "https" && registrable(to.Hostname()) != registrable(domain) {
			untrusted = true
		}
		at = to
	}
	return at, untrusted
}

type cookieJar struct {
	order []string
	vals  map[string]*http.Cookie
}

func (j *cookieJar) set(c *http.Cookie, host string) {
	d := strings.ToLower(c.Domain)
	if d == "" {
		d = host
	}
	d = strings.TrimPrefix(d, ".")
	c.Domain = d
	k := d + "|" + c.Name
	if _, ok := j.vals[k]; !ok {
		j.order = append(j.order, k)
	}
	j.vals[k] = c
}

func (j *cookieJar) header(host string) string {
	var parts []string
	for _, k := range j.order {
		c := j.vals[k]
		if host == c.Domain || strings.HasSuffix(host, "."+c.Domain) {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

func isRedirectStatus(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// followed = _getFollowing's return: the last response, unread.
type followed struct {
	resp      *http.Response
	cancel    context.CancelFunc
	locations []*url.URL
	limitHit  bool
}

// getFollowing walks redirects by hand carrying cookies like a browser.
func getFollowing(ctx context.Context, client *http.Client, start *url.URL) (*followed, error) {
	jar := &cookieJar{vals: map[string]*http.Cookie{}}
	var locations []*url.URL
	at := start
	for hop := 0; ; hop++ {
		rctx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(rctx, "GET", at.String(), nil)
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("User-Agent", androidUA)
		host := strings.ToLower(at.Hostname())
		if h := jar.header(host); h != "" {
			req.Header.Set("Cookie", h)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		for _, c := range resp.Cookies() {
			jar.set(c, host)
		}
		loc := resp.Header.Get("Location")
		var next *url.URL
		if isRedirectStatus(resp.StatusCode) && loc != "" {
			if ref, err := parseRef(strings.TrimSpace(loc)); err == nil {
				next = at.ResolveReference(ref)
			}
		}
		if next == nil || (next.Scheme != "https" && next.Scheme != "http") {
			return &followed{resp: resp, cancel: cancel, locations: locations}, nil
		}
		if hop >= maxRedirects {
			return &followed{resp: resp, cancel: cancel, locations: locations, limitHit: true}, nil
		}
		// drain().timeout(10 s)
		t := time.AfterFunc(10*time.Second, cancel)
		_, derr := ioCopyDiscard(resp.Body)
		stopped := t.Stop()
		resp.Body.Close()
		cancel()
		if derr != nil {
			if !stopped {
				return nil, &phaseErr{"timeout", derr}
			}
			return nil, derr
		}
		ref, _ := parseRef(strings.TrimSpace(loc))
		locations = append(locations, ref)
		at = next
	}
}

// detectAntiBot: only pages that look like a stop count.
func detectAntiBot(code int, hasCode bool, h http.Header, finalURI *url.URL, body []byte) any {
	server := strings.ToLower(h.Get("Server"))
	setCookie := strings.ToLower(strings.Join(h.Values("Set-Cookie"), ";"))
	path := strings.ToLower(finalURI.Path)
	cfChallenge := strings.ToLower(h.Get("Cf-Mitigated")) == "challenge"
	captchaURL := strings.Contains(path, "captcha")
	if !cfChallenge && !captchaURL && (!hasCode || code < 400) {
		return nil
	}
	head := body
	if len(head) > 65536 {
		head = head[:65536]
	}
	text := string(asciiLower(head))
	anyOf := func(xs ...string) bool {
		for _, x := range xs {
			if strings.Contains(server, x) || strings.Contains(setCookie, x) || strings.Contains(text, x) {
				return true
			}
		}
		return false
	}
	switch {
	case cfChallenge || (strings.Contains(server, "cloudflare") && anyOf("challenge-platform", "cf-chl", "just a moment")):
		return "cloudflare"
	case anyOf("ddos-guard", "__ddg"):
		return "ddos-guard"
	case anyOf("qrator"):
		return "qrator"
	case anyOf("stormwall"):
		return "stormwall"
	case anyOf("variti"):
		return "variti"
	case anyOf("servicepipe"):
		return "servicepipe"
	case strings.Contains(path, "showcaptcha") || anyOf("smartcaptcha", "captcha.yandex"):
		return "yandex-captcha"
	case captchaURL || strings.Contains(text, "captcha"):
		return "captcha"
	}
	return nil
}

func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

var assetRe = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']([^"'#?\s]+\.(?:css|js|png|jpe?g|webp|gif|svg|woff2?|ttf|mp4|json)(?:\?[^"'\s]*)?)["']`)

func assetCandidates(domain string, page *url.URL, body string) []*url.URL {
	var out []*url.URL
	seen := map[string]bool{}
	for _, m := range assetRe.FindAllStringSubmatch(body, -1) {
		ref, err := parseRef(m[1])
		if err != nil {
			continue
		}
		u := page.ResolveReference(ref)
		if u.Scheme != "https" && u.Scheme != "http" {
			continue
		}
		if registrable(u.Hostname()) != registrable(domain) && registrable(u.Hostname()) != registrable(page.Hostname()) {
			continue
		}
		if seen[u.String()] {
			continue
		}
		seen[u.String()] = true
		out = append(out, u)
		if len(out) >= maxAssetCandidates {
			break
		}
	}
	return out
}

type pullResult struct {
	bytes             int
	complete, stalled bool
	code              int
	hasCode           bool
}

// pull fetches one asset fully (capped): "stalled" = died after some bytes
// but before stallMaxBytes -- the cutoff signature.
func pull(ctx context.Context, client *http.Client, u *url.URL) pullResult {
	var r pullResult
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", u.String(), nil)
	if err != nil {
		return r
	}
	req.Header.Set("User-Agent", androidUA)
	resp, err := client.Do(req)
	if err != nil {
		return r
	}
	defer resp.Body.Close()
	r.code, r.hasCode = resp.StatusCode, true
	err = newIdleReader(cancel, 20*time.Second).read(resp.Body, func(b []byte) bool {
		r.bytes += len(b)
		return r.bytes <= maxPayloadBytes
	})
	if err != nil {
		r.stalled = r.bytes > 0 && r.bytes < stallMaxBytes
		return r
	}
	r.complete = true
	return r
}

func checkDomain(ctx context.Context, env Env, domain string, ipFamily int) map[string]any {
	res := ipResult()
	res["ip_family"] = famOrNil(ipFamily)
	var resolved string
	if ipFamily == 6 {
		// «Через IPv6»: the domain's own AAAA. None (or only NAT64 ones) =
		// nothing to reach over IPv6 -- not a DNS failure, not a block.
		if v6, err := lookupFamily(ctx, env, domain, 6, 8*time.Second); err == nil && len(v6) > 0 {
			resolved = v6[0].String()
			if isBogusIP6(v6[0]) {
				res["resolved_ip"], res["error"], res["block_reason"] = resolved, "dns poisoned", "dns_poisoned"
				return res
			}
		}
		if resolved == "" {
			res["error"], res["block_reason"] = "no AAAA", "no_aaaa"
			return res
		}
	} else {
		ip, err := resolveV4(ctx, env, domain, 8*time.Second)
		if err != nil {
			res["error"], res["block_reason"] = "dns failed", "dns"
			return res
		}
		resolved = ip.String()
		if isBogusIP(resolved) {
			res["resolved_ip"], res["error"], res["block_reason"] = resolved, "dns poisoned", "dns_poisoned"
			return res
		}
	}

	tr := binderTransportFamily(env, ipFamily, 8*time.Second, 10*time.Second, 15*time.Second)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	start := time.Now()
	var (
		nbytes       int
		code         int
		hasCode      bool
		finalHost    any
		finalURI     = httpsRoot(domain)
		blockReason  string
		errText      string
		hasErr       bool
		body         bytes.Buffer
		pageComplete bool
		antiBot      any
		redirectLim  bool
		redirects    int
	)
	got, err := getFollowing(ctx, client, httpsRoot(domain))
	if err == nil {
		resp := got.resp
		code, hasCode = resp.StatusCode, true
		redirectLim = got.limitHit
		redirects = len(got.locations)
		var untrusted bool
		finalURI, untrusted = walkRedirects(domain, got.locations)
		if finalURI.Hostname() == "" {
			finalHost = domain
		} else {
			finalHost = finalURI.Hostname()
		}
		rerr := newIdleReader(got.cancel, 15*time.Second).read(resp.Body, func(b []byte) bool {
			nbytes += len(b)
			if body.Len() < 512*1024 {
				body.Write(b)
			}
			return nbytes <= maxPayloadBytes
		})
		resp.Body.Close()
		got.cancel()
		if rerr == nil {
			pageComplete = true
		} else if nbytes > 0 && nbytes < stallMaxBytes {
			blockReason = "volume"
		}
		fh, _ := finalHost.(string)
		if blockReason == "" && (untrusted || (finalURI.Scheme != "https" && registrable(fh) != registrable(domain))) {
			blockReason = "redirect"
		}
		if blockReason == "" || blockReason == "volume" {
			antiBot = detectAntiBot(code, hasCode, resp.Header, finalURI, body.Bytes())
			if antiBot != nil && pageComplete {
				blockReason = ""
			}
		}
	} else {
		hasErr = true
		var pe *phaseErr
		phase := ""
		if errors.As(err, &pe) {
			phase = pe.phase
		}
		switch {
		case phase == "connect":
			// HttpClient.connectionTimeout / refused / unreachable: a
			// SocketException; "reset" in its OS error reads as a TLS cut.
			errText, blockReason = shortErr(err), "connect"
			if isTimeout(pe.err) {
				errText = "HTTP connection timed out after " + fmtDartDuration(8*time.Second)
			}
			if isReset(err) {
				blockReason = "tls"
			}
		case phase == "tls":
			errText, blockReason = "HandshakeException: "+shortErr(err), "tls"
		case isTimeout(err):
			errText, blockReason = "timeout", "timeout"
		case isTLSErr(err):
			errText, blockReason = "HandshakeException: "+shortErr(err), "tls"
		default:
			errText, blockReason = shortErr(err), "connect"
			if isReset(err) {
				blockReason = "tls"
			}
		}
	}
	pageMs := time.Since(start).Milliseconds()
	ok := blockReason == "" && hasCode && (antiBot != nil || (code < 500 && (nbytes > 0 || redirectLim)))

	var payloadOK, payloadBytes, payloadURL, payloadFrom, stallBytes any
	if ok && antiBot == nil && !redirectLim {
		if pageComplete && nbytes >= minPayloadBytes {
			payloadOK, payloadBytes, payloadURL, payloadFrom = true, nbytes, finalURI.String(), "page"
		} else {
			vtr := binderTransportFamily(env, ipFamily, 8*time.Second, 10*time.Second, 15*time.Second)
			vc := &http.Client{Transport: vtr, CheckRedirect: dartRedirectPolicy(5)}
			for _, u := range assetCandidates(domain, finalURI, latin1(body.Bytes())) {
				r := pull(ctx, vc, u)
				if r.stalled {
					payloadOK, stallBytes, payloadURL, payloadFrom = false, r.bytes, u.String(), "asset"
					blockReason, ok = "volume", false
					errText, hasErr = "flow cut after "+strconv.Itoa(r.bytes)+" bytes", true
					break
				}
				if r.complete && r.hasCode && r.code < 300 && r.bytes >= minPayloadBytes {
					payloadOK, payloadBytes, payloadURL, payloadFrom = true, r.bytes, u.String(), "asset"
					break
				}
			}
			vtr.CloseIdleConnections()
		}
	} else if blockReason == "volume" {
		payloadOK, stallBytes, payloadURL, payloadFrom = false, nbytes, finalURI.String(), "page"
	}

	res["accessible"] = ok
	res["resolved_ip"] = resolved
	if ok {
		res["tcp_latency_ms"] = pageMs
	}
	res["port_443_open"] = hasCode
	res["tls_ok"] = hasCode
	if hasCode {
		res["http_code"] = code
	}
	res["bytes"] = nbytes
	res["final_host"] = finalHost
	res["final_scheme"] = finalURI.Scheme
	if !ok {
		if blockReason == "" {
			blockReason = "http"
		}
		res["block_reason"] = blockReason
		if hasErr {
			res["error"] = errText
		} else if hasCode {
			res["error"] = "HTTP " + strconv.Itoa(code)
		} else {
			res["error"] = "HTTP null"
		}
	}
	res["time_ms"] = pageMs
	res["payload_ok"] = payloadOK
	res["payload_bytes"] = payloadBytes
	res["payload_url"] = payloadURL
	res["payload_from"] = payloadFrom
	res["stall_bytes"] = stallBytes
	res["anti_bot"] = antiBot
	res["redirects"] = redirects
	res["redirect_limit"] = redirectLim
	return res
}

func ioCopyDiscard(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

// checkIP = _checkIp: a literal address, IPv4 or (1.0.1) IPv6 -- TCP 443 /
// TLS / TCP 80 straight to it (an IPv6 one necessarily over the modem's IPv6).
func checkIP(ctx context.Context, env Env, target string, family int) map[string]any {
	res := ipResult()
	res["ip_family"] = family
	var resolved any
	host := target
	if family == 6 {
		resolved = target
	} else if a, err := resolveV4(ctx, env, target, 8*time.Second); err == nil {
		resolved, host = a.String(), a.String()
	} else {
		resolved = target // literal (or unresolvable) -- still meaningful
	}
	start := time.Now()
	port443, port80, tlsOK := false, false, false
	var errText any
	if c, err := dialHost(ctx, env, host, "443", 8*time.Second); err == nil {
		port443 = true
		c.Close()
	} else {
		errText = "SocketException: " + shortErr(err)
	}
	latency := time.Since(start).Milliseconds()

	if c, err := dialHost(ctx, env, host, "443", 8*time.Second); err == nil {
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if tc.HandshakeContext(hctx) == nil {
			tlsOK = true
		}
		cancel()
		c.Close()
	}
	if c, err := dialHost(ctx, env, host, "80", 6*time.Second); err == nil {
		port80 = true
		c.Close()
	}
	accessible := port443 || port80
	res["accessible"] = accessible
	res["resolved_ip"] = resolved
	if accessible {
		res["tcp_latency_ms"] = latency
	} else {
		res["error"] = errText
	}
	res["port_80_open"] = port80
	res["port_443_open"] = port443
	res["tls_ok"] = tlsOK
	return res
}
