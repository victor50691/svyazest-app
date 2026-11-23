package checks

// Port of lib/services/build_xray_config.dart (field for field), plus
// desktopXrayConfig: what the computer needs on top -- every outbound bound
// to the modem (sockopt from netbind), xray's own DNS sent out of the modem.

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"strconv"
	"strings"
)

// buildXrayConfig = buildXrayConfig(key, localPort) of the Android app.
func buildXrayConfig(key *vpnKey, localPort int) (map[string]any, error) {
	ob, err := buildOutbound(key)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"dns": map[string]any{
			"servers":       []any{"8.8.8.8", "1.1.1.1", "localhost"},
			"queryStrategy": "UseIPv4",
		},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": localPort, "protocol": "http", "settings": map[string]any{}},
		},
		"outbounds": []any{ob},
	}, nil
}

type errUnsupportedProtocol string

func (e errUnsupportedProtocol) Error() string { return "Unsupported protocol: " + string(e) }

func nonEmpty(p map[string]string, k string) bool { return p[k] != "" }

func pick(p map[string]string, k, def string) string {
	if v := p[k]; v != "" {
		return v
	}
	return def
}

func buildOutbound(key *vpnKey) (map[string]any, error) {
	p := key.Params
	host := key.Address
	switch key.Protocol {
	case "vless":
		user := map[string]any{"id": key.UUID, "encryption": "none"}
		if nonEmpty(p, "flow") {
			user["flow"] = p["flow"]
		}
		if nonEmpty(p, "encryption") {
			user["encryption"] = p["encryption"]
		}
		stream := map[string]any{}
		applyTransport(stream, key.network(), p, host)
		security := "none"
		if v, ok := p["security"]; ok {
			security = v
		}
		if security == "reality" {
			applyReality(stream, p, host)
		} else if security == "tls" {
			applyTLS(stream, p, host)
		}
		applyFinalmask(stream, p)
		ob := map[string]any{
			"protocol": "vless",
			"settings": map[string]any{"vnext": []any{
				map[string]any{"address": host, "port": key.Port, "users": []any{user}},
			}},
			"streamSettings": stream,
		}
		applyMux(ob, p)
		return ob, nil

	case "vmess":
		stream := map[string]any{}
		applyTransport(stream, key.network(), p, host)
		if p["security"] == "tls" {
			applyTLS(stream, p, host)
		}
		applyFinalmask(stream, p)
		ob := map[string]any{
			"protocol": "vmess",
			"settings": map[string]any{"vnext": []any{
				map[string]any{"address": host, "port": key.Port, "users": []any{
					map[string]any{"id": key.UUID, "alterId": key.AlterID, "security": key.VmessSec},
				}},
			}},
			"streamSettings": stream,
		}
		applyMux(ob, p)
		return ob, nil

	case "trojan":
		stream := map[string]any{}
		applyTransport(stream, key.network(), p, host)
		security := "tls"
		if v, ok := p["security"]; ok {
			security = v
		}
		if security == "reality" {
			applyReality(stream, p, host)
		} else if security != "none" {
			applyTLS(stream, p, host)
		} else {
			stream["security"] = "none"
		}
		applyFinalmask(stream, p)
		ob := map[string]any{
			"protocol": "trojan",
			"settings": map[string]any{"servers": []any{
				map[string]any{"address": host, "port": key.Port, "password": key.Password},
			}},
			"streamSettings": stream,
		}
		applyMux(ob, p)
		return ob, nil

	case "shadowsocks":
		stream := map[string]any{}
		applyTransport(stream, key.network(), p, host)
		if p["security"] == "tls" {
			applyTLS(stream, p, host)
		}
		applyFinalmask(stream, p)
		return map[string]any{
			"protocol": "shadowsocks",
			"settings": map[string]any{"servers": []any{
				map[string]any{"address": host, "port": key.Port, "method": key.Method, "password": key.Password},
			}},
			"streamSettings": stream,
		}, nil

	case "hysteria2":
		stream := map[string]any{
			"network":  "hysteria",
			"security": "tls",
			"tlsSettings": map[string]any{
				"serverName": pick(p, "sni", host),
				"alpn":       []any{"h3"},
			},
			"hysteriaSettings": map[string]any{"auth": key.Password, "version": 2},
		}
		if fm := hy2Finalmask(p); fm != nil {
			stream["finalmask"] = fm
		}
		return map[string]any{
			"protocol":       "hysteria",
			"settings":       map[string]any{"address": host, "port": key.Port, "version": 2},
			"streamSettings": stream,
		}, nil
	}
	return nil, errUnsupportedProtocol(key.Protocol)
}

func applyTransport(stream map[string]any, rawNetwork string, p map[string]string, host string) {
	network := rawNetwork
	if network == "" {
		network = "tcp"
	}
	switch {
	case (network == "tcp" || network == "raw") && p["headerType"] == "http":
		request := map[string]any{"path": []any{pick(p, "path", "/")}}
		if nonEmpty(p, "host") {
			hs := []any{}
			for _, h := range strings.Split(p["host"], ",") {
				hs = append(hs, h)
			}
			request["headers"] = map[string]any{"Host": hs}
		}
		stream["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": request}}
		network = "tcp"
	case network == "ws":
		headers := map[string]any{}
		if nonEmpty(p, "host") {
			headers["Host"] = p["host"]
		}
		stream["wsSettings"] = map[string]any{"path": pick(p, "path", "/"), "headers": headers}
	case network == "grpc":
		// serviceName ?? path ?? '' -- null-coalescing, an empty serviceName stays.
		sn, ok := p["serviceName"]
		if !ok {
			sn = p["path"]
		}
		grpc := map[string]any{"serviceName": sn, "multiMode": p["mode"] == "multi"}
		if nonEmpty(p, "authority") {
			grpc["authority"] = p["authority"]
		}
		stream["grpcSettings"] = grpc
	case network == "xhttp":
		xhttp := map[string]any{
			"path": pick(p, "path", "/"),
			"host": pick(p, "host", host),
			"mode": pick(p, "mode", "auto"),
		}
		if extra := p["extra"]; extra != "" {
			if v, ok := decodeJSONNumbers(extra); ok {
				xhttp["extra"] = sanitizeXhttpExtra(v)
			}
		}
		stream["xhttpSettings"] = xhttp
	case network == "httpupgrade":
		stream["httpupgradeSettings"] = map[string]any{"path": pick(p, "path", "/"), "host": pick(p, "host", host)}
	case network == "kcp" || network == "mkcp":
		network = "mkcp"
	}
	stream["network"] = network
}

// decodeJSONNumbers = jsonDecode, numbers kept as their literal.
func decodeJSONNumbers(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing garbage: jsonDecode throws
	}
	return v, true
}

// sanitizeXhttpExtra: Dart turns whole-number doubles (0.0) into ints; Xray
// rejects 0.0 for its integer ranges. Integer literals stay as written.
func sanitizeXhttpExtra(v any) any {
	switch x := v.(type) {
	case json.Number:
		lit := string(x)
		if !strings.ContainsAny(lit, ".eE") {
			return x
		}
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return x
		}
		if f == math.Round(f) && !math.IsInf(f, 0) {
			if f >= -9.2e18 && f <= 9.2e18 {
				return int64(f)
			}
		}
		return f
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = sanitizeXhttpExtra(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sanitizeXhttpExtra(e)
		}
		return out
	}
	return v
}

func applyTLS(stream map[string]any, p map[string]string, host string) {
	sni := pick(p, "sni", pick(p, "host", host))
	tls := map[string]any{"serverName": sni}
	if nonEmpty(p, "alpn") {
		al := []any{}
		for _, a := range strings.Split(p["alpn"], ",") {
			al = append(al, a)
		}
		tls["alpn"] = al
	}
	if nonEmpty(p, "fp") {
		tls["fingerprint"] = p["fp"]
	}
	if nonEmpty(p, "ech") {
		tls["echConfigList"] = p["ech"]
	}
	stream["security"] = "tls"
	stream["tlsSettings"] = tls
}

func applyReality(stream map[string]any, p map[string]string, host string) {
	r := map[string]any{
		"serverName":  pick(p, "sni", host),
		"fingerprint": pick(p, "fp", "chrome"),
		"publicKey":   p["pbk"],
		"shortId":     p["sid"],
		"spiderX":     pick(p, "spx", "/"),
	}
	if nonEmpty(p, "pqv") {
		r["mldsa65Verify"] = p["pqv"]
	}
	stream["security"] = "reality"
	stream["realitySettings"] = r
}

func applyMux(ob map[string]any, p map[string]string) {
	switch p["mux"] {
	case "1", "true", "on":
		c, ok := dartTryParseInt(p["muxConcurrency"])
		if !ok {
			c = 8
		}
		ob["mux"] = map[string]any{"enabled": true, "concurrency": c}
	}
}

// ---- finalmask (not in the Android 1.17.0 builder; see the report) ----
//
// The bot (telegram-bot services/vpn_checker.py _stream_finalmask /
// _hy2_finalmask) turns pasted JSON configs into share links with the whole
// streamSettings.finalmask in `fm=`, and picks xray 26.10 for a key exactly
// when that JSON needs it (needs_core). Dialing such a key without it
// would test a different channel than the key describes.

func applyFinalmask(stream map[string]any, p map[string]string) {
	if p["fm"] == "" {
		return
	}
	v, ok := decodeJSONNumbers(p["fm"])
	if !ok {
		return
	}
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		stream["finalmask"] = m
	}
}

func hy2Finalmask(p map[string]string) map[string]any {
	fm := map[string]any{}
	if p["fm"] != "" {
		if v, ok := decodeJSONNumbers(p["fm"]); ok {
			if m, ok := v.(map[string]any); ok {
				fm = m
			}
		}
	}
	if strings.ToLower(p["obfs"]) == "salamander" && p["obfs-password"] != "" {
		udp, _ := fm["udp"].([]any)
		has := false
		for _, e := range udp {
			if m, ok := e.(map[string]any); ok && m["type"] == "salamander" {
				has = true
			}
		}
		if !has {
			udp = append(udp, map[string]any{"type": "salamander", "settings": map[string]any{"password": p["obfs-password"]}})
		}
		fm["udp"] = udp
	}
	if len(fm) == 0 {
		return nil
	}
	return fm
}

// ---- desktop ----

const (
	dnsTag    = "svz-dns"
	directTag = "svz-direct"
)

// desktopXrayConfig takes the Android config and makes it leave through the
// modem only: sockopt from the binder merged into every outbound, the VPN
// server name resolved by xray's own DNS (domainStrategy) whose queries go
// out of a direct outbound bound to the modem, never the system resolver
// ('localhost' dropped: it would ask over the default route).
func desktopXrayConfig(cfg map[string]any, sockopt map[string]any, dnsServers []netip.Addr) map[string]any {
	servers := []any{}
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			servers = append(servers, s)
		}
	}
	for _, a := range dnsServers {
		if a.IsValid() && !a.IsLoopback() && !a.IsUnspecified() {
			add(a.Unmap().String())
		}
	}
	if dns, ok := cfg["dns"].(map[string]any); ok {
		if old, ok := dns["servers"].([]any); ok {
			for _, s := range old {
				if str, ok := s.(string); ok && str != "localhost" {
					add(str)
				}
			}
		}
		dns["servers"] = servers
		dns["tag"] = dnsTag
	}
	obs, _ := cfg["outbounds"].([]any)
	obs = append(obs, map[string]any{
		"tag":      directTag,
		"protocol": "freedom",
		"settings": map[string]any{"domainStrategy": "AsIs"},
	})
	for _, o := range obs {
		ob, ok := o.(map[string]any)
		if !ok {
			continue
		}
		stream, _ := ob["streamSettings"].(map[string]any)
		if stream == nil {
			stream = map[string]any{}
			ob["streamSettings"] = stream
		}
		so, _ := stream["sockopt"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		for k, v := range sockopt {
			so[k] = v
		}
		if _, ok := so["domainStrategy"]; !ok && ob["tag"] != directTag {
			so["domainStrategy"] = "UseIPv4"
		}
		stream["sockopt"] = so
	}
	cfg["outbounds"] = obs
	cfg["routing"] = map[string]any{
		"domainStrategy": "AsIs",
		"rules": []any{
			map[string]any{"type": "field", "inboundTag": []any{dnsTag}, "outboundTag": directTag},
		},
	}
	return cfg
}

func marshalConfig(cfg map[string]any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(cfg)
	return b.Bytes()
}
