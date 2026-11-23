package checks

// Network plumbing shared by the checks. Every socket towards a target
// leaves through env.Net (the modem); the only exception is the loopback hop
// to the local xray inbound (localDial), which must NOT be bound to the
// modem (a socket pinned to wwan0 cannot reach 127.0.0.1).

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const androidUA = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36"

// resolveV4 = InternetAddress.lookup(host, type: IPv4) over the modem.
func resolveV4(ctx context.Context, env Env, host string, timeout time.Duration) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
		return netip.Addr{}, errors.New("not an IPv4 address")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addrs, err := env.Net.LookupHost(ctx, host)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
	}
	return netip.Addr{}, errors.New("no IPv4 address")
}

// isNat64 = _isNat64: 64:ff9b::/96, what a DNS64 resolver synthesizes for
// an IPv4-only site -- not the site's own IPv6.
func isNat64(a netip.Addr) bool {
	if !a.Is6() || a.Is4In6() {
		return false
	}
	b := a.As16()
	if b[0] != 0x00 || b[1] != 0x64 || b[2] != 0xff || b[3] != 0x9b {
		return false
	}
	for _, x := range b[4:12] {
		if x != 0 {
			return false
		}
	}
	return true
}

func familyOf(a netip.Addr) int {
	if a.Unmap().Is4() {
		return 4
	}
	return 6
}

// lookupFamily = _lookupFamily (Android 1.17.2): the addresses of host in
// family (4/6) only, from the modem's DNS (env.Net.LookupHost asks A+AAAA,
// the other family is dropped here); NAT64 answers do not count for 6. A
// literal is its own family.
func lookupFamily(ctx context.Context, env Env, host string, family int, timeout time.Duration) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if familyOf(a) == family {
			return []netip.Addr{a.Unmap()}, nil
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addrs, err := env.Net.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if familyOf(a) != family || (family == 6 && isNat64(a)) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// dialHost dials host:port out of the modem; a name is resolved by the
// modem's DNS (env.Net.LookupHost), IPv4 addresses tried first.
func dialHost(ctx context.Context, env Env, host string, port string, timeout time.Duration) (net.Conn, error) {
	return dialHostFamily(ctx, env, host, port, timeout, 0)
}

// dialHostFamily: family 4 / 6 = only addresses of that family (the job's
// ip_family, _pinFamily in ip_checker.dart); 0 = IPv4 first, then IPv6.
func dialHostFamily(ctx context.Context, env Env, host string, port string, timeout time.Duration, family int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if family == 4 || family == 6 {
		addrs, err := lookupFamily(ctx, env, host, family, timeout)
		if err != nil {
			return nil, &net.DNSError{Err: err.Error(), Name: host}
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no IPv%d address for the host", family)
		}
		network := "tcp4"
		if family == 6 {
			network = "tcp6"
		}
		var lastErr error
		for _, a := range addrs {
			c, err := env.Net.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return env.Net.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
	}
	addrs, err := env.Net.LookupHost(ctx, host)
	if err != nil {
		return nil, &net.DNSError{Err: err.Error(), Name: host}
	}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		if a.Unmap().Is4() {
			v4 = append(v4, a.Unmap())
		} else {
			v6 = append(v6, a)
		}
	}
	var lastErr error = errors.New("no addresses")
	for _, a := range append(v4, v6...) {
		c, err := env.Net.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// tcpConnectMs = NativePlugin.tcpConnect: handshake time in ms or nil.
func tcpConnectMs(ctx context.Context, env Env, ip netip.Addr, port int, timeout time.Duration) any {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	c, err := env.Net.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return nil
	}
	ms := time.Since(start).Milliseconds()
	c.Close()
	return ms
}

// icmpMs = NativePlugin.icmpPing: one echo out of the modem, ms or nil.
func icmpMs(ctx context.Context, env Env, ip netip.Addr, timeout time.Duration) any {
	ctx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	d, err := env.Net.Ping(ctx, ip, timeout)
	if err != nil {
		return nil
	}
	return d.Milliseconds()
}

// phaseErr tags where an HTTP attempt died, for Dart-like classification.
type phaseErr struct {
	phase string // connect | tls | timeout
	err   error
}

func (e *phaseErr) Error() string { return e.err.Error() }
func (e *phaseErr) Unwrap() error { return e.err }

// binderTransport: HTTP/1.1 only (Dart's HttpClient never speaks h2),
// connect within connectTimeout, connect+TLS within hopTimeout (Dart's
// getUrl().timeout), headers within headerTimeout.
func binderTransport(env Env, connectTimeout, hopTimeout, headerTimeout time.Duration) *http.Transport {
	return binderTransportFamily(env, 0, connectTimeout, hopTimeout, headerTimeout)
}

// binderTransportFamily: every connection over IP family 4 / 6 only
// (_pinFamily); 0 = as binderTransport.
func binderTransportFamily(env Env, family int, connectTimeout, hopTimeout, headerTimeout time.Duration) *http.Transport {
	dial := func(ctx context.Context, addr string) (net.Conn, string, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, "", &phaseErr{"connect", err}
		}
		c, err := dialHostFamily(ctx, env, host, port, connectTimeout, family)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", &phaseErr{"timeout", err}
			}
			return nil, "", &phaseErr{"connect", err}
		}
		return c, host, nil
	}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, hopTimeout)
			defer cancel()
			c, _, err := dial(ctx, addr)
			return c, err
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, hopTimeout)
			defer cancel()
			c, host, err := dial(ctx, addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(c, &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				if ctx.Err() != nil {
					return nil, &phaseErr{"timeout", err}
				}
				return nil, &phaseErr{"tls", err}
			}
			return tc, nil
		},
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
}

// proxyTransport: through the local xray HTTP inbound; certificates are
// not checked (badCertificateCallback => true in vpn_checker.dart).
func proxyTransport(localPort int, connectTimeout, tlsTimeout, headerTimeout time.Duration) *http.Transport {
	pu := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))}
	d := &net.Dialer{Timeout: connectTimeout}
	return &http.Transport{
		Proxy:                 http.ProxyURL(pu),
		DialContext:           d.DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
}

// errRedirect = Dart's RedirectException (limit exceeded / loop).
var errRedirect = errors.New("RedirectException")

// dartRedirectPolicy: followRedirects with maxRedirects=max, and Dart's
// "Redirect loop detected" on a URL already visited.
func dartRedirectPolicy(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > max {
			return errRedirect
		}
		for _, v := range via {
			if v.URL.String() == req.URL.String() {
				return errRedirect
			}
		}
		return nil
	}
}

// idleReader reads body chunks; each read must return within idle (Dart
// Stream.timeout), the request is cancelled otherwise.
type idleReader struct {
	cancel  context.CancelFunc
	idle    time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func newIdleReader(cancel context.CancelFunc, idle time.Duration) *idleReader {
	r := &idleReader{cancel: cancel, idle: idle}
	r.timer = time.AfterFunc(idle, func() { r.expired.Store(true); cancel() })
	return r
}

// read consumes body; fn gets each chunk and may return false to stop.
func (r *idleReader) read(body io.Reader, fn func([]byte) bool) error {
	defer r.timer.Stop()
	buf := make([]byte, 32*1024)
	for {
		r.timer.Reset(r.idle)
		n, err := body.Read(buf)
		if n > 0 {
			if !fn(buf[:n]) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if r.expired.Load() {
				return &phaseErr{"timeout", err}
			}
			return err
		}
	}
}

func isTimeout(err error) bool {
	var pe *phaseErr
	if errors.As(err, &pe) && pe.phase == "timeout" {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "reset")
}

func isTLSErr(err error) bool {
	var pe *phaseErr
	if errors.As(err, &pe) && pe.phase == "tls" {
		return true
	}
	var rh tls.RecordHeaderError
	var ae tls.AlertError
	var ce *tls.CertificateVerificationError
	return errors.As(err, &rh) || errors.As(err, &ae) || errors.As(err, &ce)
}

// dartErrType = e.runtimeType.toString() of what dart:io would have thrown.
func dartErrType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errRedirect):
		return "RedirectException"
	case isTimeout(err):
		return "TimeoutException"
	case isTLSErr(err):
		return "HandshakeException"
	}
	var pe *phaseErr
	if errors.As(err, &pe) && pe.phase == "connect" {
		return "SocketException"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "HttpException"
	}
	return "SocketException"
}

// shortErr: a one-line error text without Go's "Get \"url\":" prefix (the
// URL may be the customer's target).
func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	// "read tcp 10.64.1.2:48474->1.2.3.4:443: ..." -> without our own
	// (the partner's modem) address; the result reaches the customer.
	return localAddrRe.ReplaceAllString(s, "")
}

var localAddrRe = regexp.MustCompile(`\S+->`)

func fmtDartDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := d.Seconds() - float64(h*3600+m*60)
	return fmt.Sprintf("%d:%02d:%09.6f", h, m, s)
}
