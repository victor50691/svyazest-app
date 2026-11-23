package checks

// Small re-implementations of the Dart core-library behaviour the Android
// parser leans on (Uri.parse, Uri.decodeComponent, Uri.splitQueryString,
// int.tryParse), so the same share link yields the same fields here.

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// dartURI is what dart:core Uri.parse exposes for an authority URI.
type dartURI struct {
	Scheme   string
	UserInfo string // raw (still percent-encoded)
	Host     string // lower-cased, IPv6 without brackets
	Port     int
	HasPort  bool
	Path     string
	Query    string // raw, without '?'
	Fragment string // raw, without '#'
}

// dartParseURI mirrors Uri.parse for "scheme://[userinfo@]host[:port]..."
// links. Errors where Dart throws a FormatException (bad port, bad IPv6).
func dartParseURI(s string) (*dartURI, error) {
	i := strings.Index(s, "://")
	if i <= 0 {
		return nil, errors.New("FormatException: Invalid URI")
	}
	u := &dartURI{Scheme: strings.ToLower(s[:i])}
	rest := s[i+3:]
	if h := strings.IndexByte(rest, '#'); h >= 0 {
		u.Fragment = rest[h+1:]
		rest = rest[:h]
	}
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		u.Query = rest[q+1:]
		rest = rest[:q]
	}
	auth := rest
	if p := strings.IndexByte(rest, '/'); p >= 0 {
		auth = rest[:p]
		u.Path = rest[p:]
	}
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		u.UserInfo = auth[:at]
		auth = auth[at+1:]
	}
	hostPart, portPart := auth, ""
	hasColon := false
	if strings.HasPrefix(auth, "[") {
		end := strings.IndexByte(auth, ']')
		if end < 0 {
			return nil, errors.New("FormatException: Missing end `]` to match `[` in host")
		}
		v6 := auth[1:end]
		if zi := strings.Index(v6, "%25"); zi >= 0 {
			v6 = v6[:zi]
		}
		if a, err := netip.ParseAddr(v6); err != nil || !a.Is6() {
			return nil, errors.New("FormatException: Illegal IPv6 address")
		}
		hostPart = auth[1:end]
		after := auth[end+1:]
		if after != "" {
			if after[0] != ':' {
				return nil, errors.New("FormatException: Invalid end of authority")
			}
			hasColon, portPart = true, after[1:]
		}
	} else if c := strings.IndexByte(auth, ':'); c >= 0 {
		hostPart, portPart, hasColon = auth[:c], auth[c+1:], true
	}
	u.Host = strings.ToLower(hostPart)
	if hasColon && portPart != "" {
		for _, ch := range portPart {
			if ch < '0' || ch > '9' {
				return nil, errors.New("FormatException: Invalid port")
			}
		}
		n, err := strconv.Atoi(portPart)
		if err != nil {
			return nil, errors.New("FormatException: Invalid port")
		}
		u.Port, u.HasPort = n, true
	}
	return u, nil
}

// lenientDecode decodes a component Uri.parse already normalized: there an
// invalid '%' had been escaped to %25 and non-ASCII to UTF-8 escapes, so
// decoding never fails on those -- only on escapes that are not UTF-8.
func lenientDecode(s string, plusToSpace bool) (string, error) {
	if !strings.ContainsAny(s, "%+") {
		return s, nil
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		case c == '+' && plusToSpace:
			b = append(b, ' ')
		default:
			b = append(b, c)
		}
	}
	if !utf8.Valid(b) {
		return "", errors.New("FormatException: Invalid UTF-8 byte")
	}
	return string(b), nil
}

// strictDecode = Uri.decodeComponent / decodeQueryComponent on a raw
// string: throws on non-ASCII, a truncated or non-hex escape, bad UTF-8.
func strictDecode(s string, plusToSpace bool) (string, error) {
	simple := true
	for i := 0; i < len(s); i++ {
		if s[i] > 127 || s[i] == '%' || (plusToSpace && s[i] == '+') {
			simple = false
			break
		}
	}
	if simple {
		return s, nil
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > 127 {
			return "", errors.New("Invalid argument(s): Illegal percent encoding in URI")
		}
		if c == '%' {
			if i+3 > len(s) {
				return "", errors.New("Invalid argument(s): Truncated URI")
			}
			if !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", errors.New("Invalid argument(s): Invalid URL encoding")
			}
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		if plusToSpace && c == '+' {
			b = append(b, ' ')
			continue
		}
		b = append(b, c)
	}
	if !utf8.Valid(b) {
		return "", errors.New("FormatException: Invalid UTF-8 byte")
	}
	return string(b), nil
}

// splitQuery = Uri.splitQueryString: '&'-separated, the LAST value of a
// repeated key wins, "=v" (empty key) is skipped, "k" alone maps to "".
func splitQuery(q string, decode func(string, bool) (string, error)) (map[string]string, error) {
	out := map[string]string{}
	for _, el := range strings.Split(q, "&") {
		eq := strings.IndexByte(el, '=')
		if eq == -1 {
			if el != "" {
				k, err := decode(el, true)
				if err != nil {
					return nil, err
				}
				out[k] = ""
			}
			continue
		}
		if eq == 0 {
			continue
		}
		k, err := decode(el[:eq], true)
		if err != nil {
			return nil, err
		}
		v, err := decode(el[eq+1:], true)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// dartTryParseInt = int.tryParse: surrounding whitespace allowed, optional
// sign, decimal or 0x-hex, 64-bit range.
func dartTryParseInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	body := s
	if body[0] == '+' || body[0] == '-' {
		neg = body[0] == '-'
		body = body[1:]
	}
	base := 10
	if len(body) > 2 && (body[:2] == "0x" || body[:2] == "0X") {
		base, body = 16, body[2:]
	}
	if body == "" {
		return 0, false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if base == 10 && (c < '0' || c > '9') {
			return 0, false
		}
		if base == 16 && !isHex(c) {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(body, base, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return int(n), true
}

// dartString = Object.toString() of a decoded JSON value, the way the
// vmess parser stringifies every field ((v ?? ”).toString()).
func dartString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case jsonNumber:
		return dartNumberString(string(x))
	default:
		return fmt.Sprint(x)
	}
}

// dartNumberString: jsonDecode keeps integers as int ("443") and anything
// with a fraction/exponent as double, whose toString always carries ".0".
func dartNumberString(lit string) string {
	if !strings.ContainsAny(lit, ".eE") {
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return lit
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
