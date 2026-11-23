package checks

// Port of lib/services/vpn_uri_parser.dart: VLESS/VMESS/TROJAN/SHADOWSOCKS/
// HYSTERIA2 share links -> the normalized shape buildXrayConfig consumes.
// Everything the link carries stays in Params verbatim.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

type jsonNumber = json.Number

type vpnKey struct {
	Protocol    string // vless | vmess | trojan | shadowsocks | hysteria2 (or the unsupported scheme)
	Raw         string
	Unsupported string // untestable reason ("" = none)

	Address    string
	HasAddress bool
	Port       int
	HasPort    bool
	UUID       string // vless / vmess
	Password   string // trojan / shadowsocks / hysteria2 auth
	Method     string // shadowsocks cipher
	AlterID    int    // vmess
	VmessSec   string // vmess "scy"
	Params     map[string]string
}

// network = params['type']?.toLowerCase() ?? 'tcp'
func (k *vpnKey) network() string {
	if v, ok := k.Params["type"]; ok {
		return strings.ToLower(v)
	}
	return "tcp"
}

func (k *vpnKey) supported() bool {
	return k.Unsupported == "" && k.HasAddress && k.HasPort
}

var (
	schemeRe = regexp.MustCompile(`^([a-zA-Z0-9+.-]+)://`)
	hy2Re    = regexp.MustCompile(`(?i)^hy2://`)
)

var unsupportedSchemes = map[string]bool{
	"tuic": true, "juicity": true, "hysteria": true,
	"wireguard": true, "wg": true, "ssr": true, "snell": true,
}

var removedTransports = map[string]bool{"h2": true, "http": true, "quic": true}

const (
	reasonSchemeUnsupported = "scheme_unsupported_by_core"
	reasonTransportRemoved  = "transport_removed_from_core"
	reasonMkcpRemoved       = "mkcp_header_seed_removed_from_core"
)

func usesRemovedMkcpFeatures(p map[string]string) bool {
	header := strings.ToLower(p["headerType"])
	return p["seed"] != "" || (header != "" && header != "none")
}

// parseVpnURI = parseVpnUri: nil for "not a share link we know at all".
func parseVpnURI(uri string) *vpnKey {
	m := schemeRe.FindStringSubmatch(uri)
	if m == nil {
		return nil
	}
	scheme := strings.ToLower(m[1])
	if unsupportedSchemes[scheme] {
		return &vpnKey{Protocol: scheme, Raw: uri, Unsupported: reasonSchemeUnsupported + ":" + scheme}
	}
	var (
		key *vpnKey
		err error
	)
	switch scheme {
	case "vless":
		key, err = parseURIKey(uri, "vless", false)
	case "trojan":
		key, err = parseURIKey(uri, "trojan", true)
	case "vmess":
		key, err = parseVmess(uri)
	case "ss", "shadowsocks":
		key, err = parseShadowsocks(uri)
	case "hysteria2", "hy2":
		key, err = parseURIKey(hy2Re.ReplaceAllLiteralString(uri, "hysteria2://"), "hysteria2", true)
		if key != nil {
			key.Raw = uri
		}
	default:
		return nil
	}
	if err != nil {
		return &vpnKey{Protocol: scheme, Raw: uri, Unsupported: "parse_error:" + err.Error()}
	}
	transport := key.network()
	if removedTransports[transport] {
		return &vpnKey{Protocol: key.Protocol, Raw: uri, Unsupported: reasonTransportRemoved + ":" + transport}
	}
	if (transport == "kcp" || transport == "mkcp") && usesRemovedMkcpFeatures(key.Params) {
		return &vpnKey{Protocol: key.Protocol, Raw: uri, Unsupported: reasonMkcpRemoved + ":" + transport}
	}
	return key
}

// vless / trojan / hysteria2: userinfo is the uuid (vless) or the password.
func parseURIKey(raw, proto string, userIsPassword bool) (*vpnKey, error) {
	u, err := dartParseURI(raw)
	if err != nil {
		return nil, err
	}
	user, err := lenientDecode(u.UserInfo, false)
	if err != nil {
		return nil, err
	}
	params, err := splitQuery(u.Query, lenientDecode)
	if err != nil {
		return nil, err
	}
	k := &vpnKey{Protocol: proto, Raw: raw, Address: u.Host, HasAddress: true, Port: 443, HasPort: true, Params: params, VmessSec: "auto"}
	if u.HasPort {
		k.Port = u.Port
	}
	if userIsPassword {
		k.Password = user
	} else {
		k.UUID = user
	}
	return k, nil
}

// b64Bytes: either alphabet, padded or not (_b64Bytes).
func b64Bytes(blob string) ([]byte, error) {
	n := strings.TrimSpace(strings.NewReplacer("-", "+", "_", "/").Replace(blob))
	n = strings.TrimRight(n, "=")
	if len(n)%4 == 1 {
		return nil, errors.New("FormatException: Invalid length")
	}
	b, err := base64.RawStdEncoding.DecodeString(n)
	if err != nil {
		return nil, errors.New("FormatException: Invalid character")
	}
	return b, nil
}

func utf8Decode(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", errors.New("FormatException: Unexpected extension byte")
	}
	return string(b), nil
}

// vmess://BASE64({v,ps,add,port,id,aid,scy,net,type,host,path,tls,sni,alpn,fp})
func parseVmess(raw string) (*vpnKey, error) {
	b, err := b64Bytes(raw[len("vmess://"):])
	if err != nil {
		return nil, err
	}
	if _, err := utf8Decode(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var j map[string]any
	if err := dec.Decode(&j); err != nil || j == nil {
		return nil, errors.New("FormatException: not a JSON object")
	}
	if dec.More() {
		return nil, errors.New("FormatException: Unexpected character")
	}
	s := func(k string) string { return dartString(j[k]) }
	tlsField := strings.ToLower(s("tls"))
	security := "none"
	if tlsField == "tls" || tlsField == "true" || tlsField == "1" {
		security = "tls"
	}
	params := map[string]string{
		"type":        ifEmpty(s("net"), "tcp"),
		"headerType":  ifEmpty(s("type"), "none"),
		"path":        s("path"),
		"host":        s("host"),
		"sni":         ifEmpty(s("sni"), s("host")),
		"security":    security,
		"alpn":        s("alpn"),
		"fp":          s("fp"),
		"serviceName": s("path"),
	}
	for k, v := range params {
		if v == "" {
			delete(params, k)
		}
	}
	k := &vpnKey{Protocol: "vmess", Raw: raw, UUID: s("id"), Address: s("add"), HasAddress: true,
		Port: 443, HasPort: true, VmessSec: ifEmpty(s("scy"), "auto"), Params: params}
	if p, ok := dartTryParseInt(s("port")); ok {
		k.Port = p
	}
	if a, ok := dartTryParseInt(s("aid")); ok {
		k.AlterID = a
	}
	return k, nil
}

var bracketRe = regexp.MustCompile(`^\[|\]$`)

// ss://BASE64(method:password)@host:port?params#tag, the legacy
// ss://BASE64(method:password@host:port) form, and plain method:password@.
func parseShadowsocks(raw string) (*vpnKey, error) {
	body := raw[strings.Index(raw, "://")+3:]
	tag := ""
	withoutTag := body
	if h := strings.IndexByte(body, '#'); h >= 0 {
		tag, withoutTag = body[h+1:], body[:h]
	}
	query := ""
	if q := strings.IndexByte(withoutTag, '?'); q >= 0 {
		query, withoutTag = withoutTag[q+1:], withoutTag[:q]
	}
	var userInfo, hostPort string
	if strings.Contains(withoutTag, "@") {
		at := strings.LastIndexByte(withoutTag, '@')
		ui, err := strictDecode(withoutTag[:at], false)
		if err != nil {
			return nil, err
		}
		userInfo, hostPort = ui, withoutTag[at+1:]
		if !strings.Contains(userInfo, ":") {
			b, err := b64Bytes(userInfo)
			if err != nil {
				return nil, err
			}
			if userInfo, err = utf8Decode(b); err != nil {
				return nil, err
			}
		}
	} else {
		b, err := b64Bytes(withoutTag)
		if err != nil {
			return nil, err
		}
		decoded, err := utf8Decode(b)
		if err != nil {
			return nil, err
		}
		at := strings.LastIndexByte(decoded, '@')
		if at < 0 {
			return nil, errors.New("RangeError: no '@' in the decoded link")
		}
		userInfo, hostPort = decoded[:at], decoded[at+1:]
	}
	method, password := "aes-256-gcm", userInfo
	if c := strings.IndexByte(userInfo, ':'); c >= 0 {
		method, password = userInfo[:c], userInfo[c+1:]
	}
	pc := strings.LastIndexByte(hostPort, ':')
	if pc < 0 {
		return nil, errors.New("RangeError: no port in the link")
	}
	host := bracketRe.ReplaceAllString(hostPort[:pc], "")
	port, hasPort := dartTryParseInt(hostPort[pc+1:])
	params := map[string]string{}
	if query != "" {
		var err error
		if params, err = splitQuery(query, strictDecode); err != nil {
			return nil, err
		}
	}
	if tag != "" {
		// Only the name, but Uri.decodeComponent throws on a raw non-ASCII or
		// broken tag and the whole key becomes parse_error -- same here.
		if _, err := strictDecode(tag, false); err != nil {
			return nil, err
		}
	}
	return &vpnKey{Protocol: "shadowsocks", Raw: raw, Method: method, Password: password,
		Address: host, HasAddress: true, Port: port, HasPort: hasPort, Params: params, VmessSec: "auto"}, nil
}

func ifEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
