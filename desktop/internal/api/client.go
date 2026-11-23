// Package api: the DPI//Checker executor API (/executor/v1/*) as the
// Android app's lib/services/api_client.dart talks to it, over
// netbind.Control (Ethernet/Wi-Fi first, the modem last).
package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultBase = "https://dpichecker.st"
const prefix = "/executor/v1"

// Doer: netbind.Control and *http.Client both fit.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RouteDoer: a Doer that knows the route it last used (netbind.Control).
type RouteDoer interface {
	Doer
	LastRoute() string
}

type Client struct {
	Base       string // "https://dpichecker.st"
	Ctl        RouteDoer
	AppVersion string // "1.0.0"
	Platform   string // "linux" | "windows"
	Token      func() string

	mu  sync.Mutex
	key *ecdsa.PrivateKey

	clockOffsetMs atomic.Int64 // server clock - local clock (Date header)
	lastRoute     atomic.Value // string
	// 1.0.9: the last answer of the server over the control channel and the
	// last failure to get one (unix ms), for «Связь с сервером».
	lastOKMs   atomic.Int64
	lastFailMs atomic.Int64
	lastFail   atomic.Value // string
	// OnRoute: called when the route to the server changes.
	OnRoute func(route string)
}

func New(base string, ctl RouteDoer, appVersion, platform string, token func() string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{Base: strings.TrimRight(base, "/"), Ctl: ctl, AppVersion: appVersion, Platform: platform, Token: token}
}

func (c *Client) SetKey(k *ecdsa.PrivateKey) {
	c.mu.Lock()
	c.key = k
	c.mu.Unlock()
}

func (c *Client) Key() *ecdsa.PrivateKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key
}

// Route: what the last request to the server went through.
func (c *Client) Route() string {
	if v, ok := c.lastRoute.Load().(string); ok && v != "" {
		return v
	}
	if c.Ctl != nil {
		if r := c.Ctl.LastRoute(); r != "—" {
			return r
		}
	}
	return ""
}

// Link: whether the last request to the server over the control channel
// got an answer (any HTTP status below 500), when, and why not. at is zero
// before the first request.
func (c *Client) Link() (ok bool, at time.Time, why string) {
	okMs, failMs := c.lastOKMs.Load(), c.lastFailMs.Load()
	if okMs == 0 && failMs == 0 {
		return false, time.Time{}, ""
	}
	if okMs >= failMs {
		return true, time.UnixMilli(okMs), ""
	}
	why, _ = c.lastFail.Load().(string)
	return false, time.UnixMilli(failMs), why
}

func (c *Client) linkFail(why string) {
	c.lastFail.Store(why)
	c.lastFailMs.Store(time.Now().UnixMilli())
}

// ServerNowMs: the server's clock as estimated from the last Date header.
func (c *Client) ServerNowMs() int64 { return time.Now().UnixMilli() + c.clockOffsetMs.Load() }

// ---- errors ----

type Error struct {
	Status  int
	Message string
	Code    string
	Body    map[string]any
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func (e *Error) Unauthorized() bool   { return e.Status == 401 }
func (e *Error) DeviceBanned() bool   { return e.Code == "hwid_banned" }
func (e *Error) AccountBanned() bool  { return e.Code == "account_banned" }
func (e *Error) CountryBlocked() bool { return e.Code == "country_blocked" }
func (e *Error) AppOutdated() bool    { return e.Code == "app_outdated" }
func (e *Error) SignatureNeeded() bool {
	return e.Code == "signature_required" || e.Code == "attestation_required"
}

// ErrNotPaired: no device token yet.
var ErrNotPaired = &Error{Status: 401, Message: "Not paired"}

// ---- request plumbing ----

type reqOpts struct {
	auth    bool
	timeout time.Duration
	doer    Doer // nil = c.Ctl
}

func (c *Client) call(ctx context.Context, method, path string, in, out any, o reqOpts) error {
	parent := ctx
	var body io.Reader
	var raw []byte
	if in != nil {
		var err error
		raw, err = json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	} else if method == http.MethodPost {
		raw = []byte("{}")
		body = bytes.NewReader(raw)
	}
	if o.timeout == 0 {
		o.timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.Base+prefix+path, body)
	if err != nil {
		return err
	}
	if raw != nil {
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		req.ContentLength = int64(len(raw))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "svyazest-agent/"+c.AppVersion+" ("+c.Platform+")")
	if c.AppVersion != "" {
		req.Header.Set("X-App-Version", c.AppVersion)
	}
	if c.Platform != "" {
		req.Header.Set("X-App-Platform", c.Platform)
	}
	doer := o.doer
	if doer == nil {
		if c.Ctl == nil {
			return errors.New("нет канала связи с сервером")
		}
		doer = c.Ctl
		// (netbind.Control sets X-Net-Route itself for the route it uses)
	} else {
		req.Header.Set("X-Net-Route", "cellular")
	}
	if o.auth {
		tok := ""
		if c.Token != nil {
			tok = c.Token()
		}
		if tok == "" {
			return ErrNotPaired
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		// 1.16.0 signing (server routes/executor/index.js requireSignature):
		// X-Sig-Ts = server ms, X-Sig = base64 DER ECDSA-SHA256 over
		// "METHOD PATH TS" with PATH = /executor/v1/jobs/... (no query).
		// /attest/desktop is signed too: re-binding when a key is already
		// bound must prove possession of the current one.
		if strings.Contains(req.URL.Path, "/jobs/") || strings.HasSuffix(req.URL.Path, "/attest/desktop") {
			if k := c.Key(); k != nil {
				ts := c.ServerNowMs()
				sig, err := SignString(k, fmt.Sprintf("%s %s %d", method, req.URL.Path, ts))
				if err == nil {
					req.Header.Set("X-Sig-Ts", strconv.FormatInt(ts, 10))
					req.Header.Set("X-Sig", sig)
				}
			}
		}
	}
	resp, err := doer.Do(req)
	if err != nil {
		// Our own stop (shutdown, logout) says nothing about the server.
		if o.doer == nil && parent.Err() == nil {
			why := "сервер недоступен"
			if errors.Is(err, context.DeadlineExceeded) {
				why = "сервер не ответил вовремя"
			}
			c.linkFail(why)
		}
		return err
	}
	defer resp.Body.Close()
	if o.doer == nil {
		if resp.StatusCode >= 500 {
			c.linkFail(fmt.Sprintf("сервер ответил ошибкой (HTTP %d)", resp.StatusCode))
		} else {
			c.lastOKMs.Store(time.Now().UnixMilli())
		}
	}
	if o.doer == nil && c.Ctl != nil {
		if r := c.Ctl.LastRoute(); r != "" && r != "—" {
			prev, _ := c.lastRoute.Load().(string)
			if r != prev {
				c.lastRoute.Store(r)
				if c.OnRoute != nil {
					c.OnRoute(r)
				}
			}
		}
	}
	if d := resp.Header.Get("Date"); d != "" {
		if t, err := http.ParseTime(d); err == nil {
			c.clockOffsetMs.Store(t.UnixMilli() - time.Now().UnixMilli())
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		e := &Error{Status: resp.StatusCode, Body: m, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		if s, ok := m["error"].(string); ok && s != "" {
			e.Message = s
		}
		if s, ok := m["code"].(string); ok {
			e.Code = s
		}
		return e
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("ответ сервера не разобран: %w", err)
	}
	return nil
}

// ---- signing ----

// SignString: base64 of the DER ECDSA-SHA256 signature over s (what Node's
// crypto.verify('sha256', data, spkiPem, sig) accepts).
func SignString(k *ecdsa.PrivateKey, s string) (string, error) {
	h := sha256.Sum256([]byte(s))
	der, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// PublicPEM: SPKI PEM ("-----BEGIN PUBLIC KEY-----").
func PublicPEM(k *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ---- tolerant JSON scalars (pg ints/numerics may come as strings) ----

type FlexInt int64

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = FlexInt(v)
	return nil
}

type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = FlexFloat(v)
	return nil
}

type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = FlexString(string(b))
	return nil
}

func q(v string) string { return url.QueryEscape(v) }
