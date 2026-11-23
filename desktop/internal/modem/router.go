package modem

// Router-style USB modems: the stick is a little NAT router with a web UI
// on its gateway address, and its HTTP API tells the operator, network type
// and signal. Asked THROUGH the stick's own interface (netbind binder), so
// a LAN router with the same address on Ethernet is never confused with it.
//
//   Huawei HiLink (E3372h, E8372, B3xx/B5xx LTE routers): 192.168.8.1,
//     GET /api/webserver/SesTokInfo -> session cookie + CSRF token, then
//     /api/net/current-plmn, /api/monitoring/status, /api/device/information.
//   ZTE (MF79x, MF833 ...): /goform/goform_get_cmd_process?cmd=... with a
//     Referer from the device itself, JSON answer.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"svyazest-agent/internal/netbind"
)

func probeRouter(ctx context.Context, d netbind.IfaceDetail, hilink, zte bool) (Info, bool) {
	b, err := netbind.ForInterface(d.Name)
	if err != nil {
		return Info{}, false
	}
	cl := b.HTTPClient(2 * time.Second)
	defer cl.CloseIdleConnections()
	gw := d.Gateway.String()
	if hilink {
		if info, ok := probeHiLink(ctx, cl, gw); ok {
			info.Interface = d.Name
			return info, true
		}
	}
	if zte {
		if info, ok := probeZTE(ctx, cl, gw); ok {
			info.Interface = d.Name
			return info, true
		}
	}
	return Info{}, false
}

func httpGet(ctx context.Context, cl *http.Client, u string, hdr map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return body, resp.Header, err
}

// ---------------------------------------------------------------- HiLink

var reXMLTag = regexp.MustCompile(`<([A-Za-z0-9_]+)>([^<]*)</[A-Za-z0-9_]+>`)

// xmlFields: flat <Tag>value</Tag> pairs of a HiLink answer; an
// <error><code>N</code> answer (e.g. 100003 "login required") is
// returned as {"code": N} so callers simply find their fields missing.
func xmlFields(b []byte) map[string]string {
	m := map[string]string{}
	for _, x := range reXMLTag.FindAllSubmatch(b, -1) {
		m[string(x[1])] = strings.TrimSpace(string(x[2]))
	}
	return m
}

func probeHiLink(ctx context.Context, cl *http.Client, gw string) (Info, bool) {
	base := "http://" + gw
	// A fresh token for every call: several firmwares make them single-use.
	get := func(path string) map[string]string {
		tok, _, err := httpGet(ctx, cl, base+"/api/webserver/SesTokInfo", nil)
		if err != nil {
			return nil
		}
		t := xmlFields(tok)
		if t["SesInfo"] == "" && t["TokInfo"] == "" {
			return nil
		}
		hdr := map[string]string{"X-Requested-With": "XMLHttpRequest"}
		if s := t["SesInfo"]; s != "" {
			hdr["Cookie"] = s
		}
		if v := t["TokInfo"]; v != "" {
			hdr["__RequestVerificationToken"] = v
		}
		body, _, err := httpGet(ctx, cl, base+path, hdr)
		if err != nil {
			return nil
		}
		return xmlFields(body)
	}
	plmn := get("/api/net/current-plmn")
	if plmn == nil {
		return Info{}, false // not a HiLink device
	}
	status := get("/api/monitoring/status")
	dev := get("/api/device/information")
	info := hilinkInfo(plmn, status, dev)
	return info, true
}

// hilinkInfo builds Info from the three answers (any may be nil / an error).
func hilinkInfo(plmn, status, dev map[string]string) Info {
	info := Info{Source: "hilink", SignalPct: -1, Model: "Huawei HiLink"}
	info.Operator = firstNonEmpty(plmn["FullName"], plmn["ShortName"])
	if n := plmn["Numeric"]; len(n) >= 5 && isDigits(n) {
		info.MCCMNC = n
		info.Country = CountryByMCC(n[:3])
	}
	info.Tech = hilinkRat(plmn["Rat"])
	if status != nil {
		if t := hilinkNetType(firstNonEmpty(status["CurrentNetworkTypeEx"], status["CurrentNetworkType"])); t != "" {
			info.Tech = t
		}
		if v, err := strconv.Atoi(status["SignalStrength"]); err == nil && v > 0 {
			info.SignalPct = clampPct(v)
		} else if v, err := strconv.Atoi(status["SignalIcon"]); err == nil {
			max := 5
			if m, err := strconv.Atoi(status["maxsignal"]); err == nil && m > 0 {
				max = m
			}
			info.SignalPct = clampPct(v * 100 / max)
		}
		info.Connected = status["ConnectionStatus"] == "901"
	}
	if dev != nil {
		if n := firstNonEmpty(dev["DeviceName"], dev["devicename"]); n != "" {
			info.Model = "Huawei " + n
		}
	}
	return info
}

// Rat of /api/net/current-plmn (3GPP access technology numbers).
func hilinkRat(s string) string {
	switch s {
	case "0", "1", "3":
		return "gsm"
	case "2", "4", "5", "6":
		return "umts"
	case "7":
		return "lte"
	case "11", "12", "13":
		return "5gnr"
	}
	return ""
}

// CurrentNetworkType(Ex) of /api/monitoring/status.
func hilinkNetType(s string) string {
	n, err := strconv.Atoi(s)
	if err != nil {
		return ""
	}
	// 1-3 GSM/GPRS/EDGE, 4-9 and 17-18 WCDMA/HSPA, 8 and 61-65 TD-SCDMA,
	// 10-16 and 21-24 CDMA/EV-DO, 41-46 UMTS (newer firmware), 19/101 LTE,
	// 1011 LTE CA, 111 NR.
	switch {
	case n == 19 || n == 101 || (n >= 1001 && n <= 1019):
		return "lte"
	case n == 111 || (n >= 1100 && n <= 1199):
		return "5gnr"
	case (n >= 1 && n <= 3) || (n >= 21 && n <= 23) || n == 13:
		return "gsm"
	case (n >= 4 && n <= 18) || (n >= 24 && n <= 65):
		return "umts"
	}
	return ""
}

// ---------------------------------------------------------------- ZTE

func probeZTE(ctx context.Context, cl *http.Client, gw string) (Info, bool) {
	q := url.Values{}
	q.Set("isTest", "false")
	q.Set("multi_data", "1")
	q.Set("cmd", "network_provider,network_provider_fullname,network_type,signalbar,ppp_status,modem_model,rmcc,rmnc,hardware_version")
	body, _, err := httpGet(ctx, cl, "http://"+gw+"/goform/goform_get_cmd_process?"+q.Encode(),
		map[string]string{"Referer": "http://" + gw + "/index.html", "X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return Info{}, false
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return Info{}, false
	}
	return zteInfo(m)
}

func zteInfo(raw map[string]any) (Info, bool) {
	m := map[string]string{}
	for k, v := range raw {
		switch x := v.(type) {
		case string:
			m[k] = strings.TrimSpace(x)
		case float64:
			m[k] = strconv.Itoa(int(x))
		}
	}
	if m["network_provider"] == "" && m["network_type"] == "" && m["modem_model"] == "" {
		return Info{}, false
	}
	info := Info{Source: "zte", SignalPct: -1, Model: "ZTE"}
	info.Operator = firstNonEmpty(m["network_provider_fullname"], m["network_provider"])
	if mcc, mnc := m["rmcc"], m["rmnc"]; len(mcc) == 3 && isDigits(mcc) && isDigits(mnc) && mnc != "" {
		if len(mnc) == 1 {
			mnc = "0" + mnc
		}
		info.MCCMNC = mcc + mnc
		info.Country = CountryByMCC(mcc)
	}
	info.Tech = normTech(m["network_type"])
	if v, err := strconv.Atoi(m["signalbar"]); err == nil {
		info.SignalPct = clampPct(v * 20)
	}
	info.Connected = m["ppp_status"] == "ppp_connected" || m["ppp_status"] == "ipv4_ipv6_connected" || m["ppp_status"] == "ipv6_connected"
	if mm := m["modem_model"]; mm != "" {
		info.Model = "ZTE " + strings.TrimPrefix(mm, "ZTE ")
	}
	return info, true
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
