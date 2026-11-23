package netbind

// The control channel: the agent's link to the DPI//Checker API, kept
// apart from the checks. The checks must go out of the modem -- that is
// what they measure; the server, under «белые списки», is usually NOT
// reachable over that same mobile network, so API requests prefer anything
// else. Same idea as the Android ControlChannel.kt (1.11.0+):
//
//  1. every other "up" interface with a default route (ethernet, wifi, ...),
//     each bound to itself;
//  2. "default": no binding, the system's own routing -- covers a VPN on
//     the host (its tun is "virtual" and never bound directly) and setups
//     where the uplink is a bridge we could not classify;
//  3. the modem, last ("cellular").
//
// A route that failed to connect in the last 60 s goes to the end (1.14.2
// on Android: preferring the last good route made the phone stick to
// cellular forever). A route is abandoned only on a connect-phase error --
// TCP or TLS failed, nothing of the request has left -- so a POST is never
// delivered twice.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	routeFailPenalty    = 60 * time.Second
	controlDialTimeout  = 8 * time.Second // as Android's connectTimeout
	controlListCacheTTL = 5 * time.Second
)

type ctlRoute struct {
	id    string // X-Net-Route value
	iface string // "" for "default"
}

type controlImpl struct {
	mu       sync.Mutex
	modem    string
	failedAt map[string]time.Time
	clients  map[string]*http.Client
	last     string

	listAt time.Time
	list   []IfaceDetail
}

func newControlImpl(modem string) Control {
	return &controlImpl{modem: modem, failedAt: map[string]time.Time{}, clients: map[string]*http.Client{}, last: "—"}
}

func (c *controlImpl) LastRoute() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *controlImpl) SetModem(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modem == name {
		return
	}
	c.modem = name
	// Bound clients may now point at the wrong role; rebuild lazily.
	for _, cl := range c.clients {
		cl.CloseIdleConnections()
	}
	c.clients = map[string]*http.Client{}
}

// routes: the order described above, recently failed ones moved last.
func (c *controlImpl) routes() []ctlRoute {
	c.mu.Lock()
	if c.list == nil || time.Since(c.listAt) > controlListCacheTTL {
		// No DNS discovery here: the bound client asks ForInterface for it
		// once, the order only needs kinds and routes.
		if l, err := listDetailedImpl(false); err == nil {
			c.list, c.listAt = l, time.Now()
		}
	}
	list, modem := c.list, c.modem
	c.mu.Unlock()

	isModem := func(d IfaceDetail) bool {
		if modem != "" {
			return d.Name == modem
		}
		return d.Kind == "modem"
	}
	var out []ctlRoute
	var cellular *ctlRoute
	for _, d := range list {
		if !d.Up || (!d.DefaultRoute && !d.Gateway.IsValid()) {
			continue
		}
		if isModem(d) {
			if cellular == nil {
				cellular = &ctlRoute{id: "cellular", iface: d.Name}
			}
			continue
		}
		switch d.Kind {
		case "virtual", "loopback":
			continue
		case "modem":
			// Another modem than the partner's chosen one: a mobile link
			// all the same, never used for the API.
			continue
		}
		out = append(out, ctlRoute{id: d.Kind + ":" + d.Name, iface: d.Name})
	}
	// Wired before wireless, then by name, so the order is stable.
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := strings.HasPrefix(out[i].id, "ethernet:"), strings.HasPrefix(out[j].id, "ethernet:")
		if wi != wj {
			return wi
		}
		return false
	})
	out = append(out, ctlRoute{id: "default"})
	if cellular != nil {
		out = append(out, *cellular)
	}
	c.mu.Lock()
	now := time.Now()
	failed := func(r ctlRoute) bool { t, ok := c.failedAt[r.id]; return ok && now.Sub(t) < routeFailPenalty }
	sort.SliceStable(out, func(i, j int) bool { return !failed(out[i]) && failed(out[j]) })
	c.mu.Unlock()
	return out
}

func (c *controlImpl) client(r ctlRoute) (*http.Client, error) {
	c.mu.Lock()
	cl := c.clients[r.id]
	c.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if r.iface == "" {
		d := &net.Dialer{Timeout: controlDialTimeout, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	} else {
		b, err := forInterfaceImpl(r.iface)
		if err != nil {
			return nil, err
		}
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, controlDialTimeout)
			defer cancel()
			return b.DialContext(ctx, network, addr)
		}
	}
	tr := newTransport(dial)
	cl = &http.Client{
		Transport: tr,
		// The API never redirects; following one would re-send the body.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.mu.Lock()
	c.clients[r.id] = cl
	c.mu.Unlock()
	return cl, nil
}

func (c *controlImpl) Do(req *http.Request) (*http.Response, error) {
	// The body must be replayable to try another route. Requests built from
	// bytes/strings readers already have GetBody; anything else is buffered.
	getBody := req.GetBody
	if req.Body != nil && req.Body != http.NoBody && getBody == nil {
		buf, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		getBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	}
	var tried []string
	for _, r := range c.routes() {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		cl, err := c.client(r)
		if err != nil {
			tried = append(tried, r.id+": "+err.Error())
			c.markFailed(r, false)
			continue
		}
		var connected atomic.Bool
		trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
		r2 := req.Clone(httptrace.WithClientTrace(req.Context(), trace))
		if getBody != nil {
			if r2.Body, err = getBody(); err != nil {
				return nil, err
			}
		}
		// Which way this request went: the server stores it next to the
		// result (partner_network.request_route), as from the phone.
		r2.Header.Set("X-Net-Route", r.id)
		resp, err := cl.Do(r2)
		if err == nil {
			c.mu.Lock()
			delete(c.failedAt, r.id)
			c.last = r.id
			c.mu.Unlock()
			return resp, nil
		}
		if connected.Load() || req.Context().Err() != nil {
			// Connected (the request may have reached the server) or the
			// caller gave up: no retry elsewhere.
			c.mu.Lock()
			c.last = r.id
			c.mu.Unlock()
			return nil, err
		}
		tried = append(tried, r.id+": "+shortErr(err))
		c.markFailed(r, true)
	}
	c.mu.Lock()
	c.last = "нет связи"
	c.mu.Unlock()
	if len(tried) == 0 {
		return nil, errors.New("нет связи с сервером: нет ни одной сети")
	}
	return nil, fmt.Errorf("нет связи с сервером ни одним путём: %s", strings.Join(tried, "; "))
}

func (c *controlImpl) markFailed(r ctlRoute, dropClient bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failedAt[r.id] = time.Now()
	if cl := c.clients[r.id]; cl != nil && dropClient {
		cl.CloseIdleConnections()
		delete(c.clients, r.id)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
