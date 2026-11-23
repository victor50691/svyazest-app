package modem

// Parsing of `netsh mbn show ...` (Windows Mobile Broadband). Kept free of
// build tags so the tests run on the Linux build box.
//
// netsh prints "    Label   : Value" lines in the UI language and in the
// console's OEM code page (CP866 on Russian Windows) when its output is a
// pipe. So: decode CP866 unless the bytes are valid UTF-8, accept English
// and Russian labels, and where possible recognise values by their shape
// (a percentage is the signal, 14-15 digits are the IMSI) instead of by
// label.

import (
	"strings"
	"unicode/utf8"
)

// decodeOEM: UTF-8 if valid, otherwise CP866 (Cyrillic DOS code page).
func decodeOEM(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c <= 0xAF: // А..Я а..п
			sb.WriteRune(rune(0x0410 + int(c) - 0x80))
		case c >= 0xE0 && c <= 0xEF: // р..я
			sb.WriteRune(rune(0x0440 + int(c) - 0xE0))
		case c == 0xF0:
			sb.WriteRune('Ё')
		case c == 0xF1:
			sb.WriteRune('ё')
		case c == 0xFF:
			sb.WriteRune(' ') // NBSP, used as thousands separator
		default:
			sb.WriteRune('?') // box drawing etc.
		}
	}
	return sb.String()
}

type kv struct{ key, val string }

// netshBlocks: blank-line separated groups of "key : value" lines.
func netshBlocks(text string) [][]kv {
	var blocks [][]kv
	var cur []kv
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		// The separator is " : " (labels never contain it, MAC values do contain ':').
		i := strings.Index(l, " : ")
		if i < 0 {
			continue // "There is 1 interface on the system:" and other headers
		}
		cur = append(cur, kv{strings.TrimSpace(l[:i]), strings.TrimSpace(l[i+3:])})
	}
	flush()
	return blocks
}

func keyIs(k string, names ...string) bool {
	k = strings.ToLower(k)
	for _, n := range names {
		if k == n {
			return true
		}
	}
	return false
}

func keyHas(k string, subs ...string) bool {
	k = strings.ToLower(k)
	for _, s := range subs {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// parseMbnInterfaces: `netsh mbn show interfaces`.
func parseMbnInterfaces(text string) []Info {
	var out []Info
	for _, blk := range netshBlocks(text) {
		info := Info{Source: "mbn", SignalPct: -1}
		var manu, model string
		isIface := false
		for i, p := range blk {
			k, v := p.key, p.val
			switch {
			case i == 0 && keyIs(k, "name", "имя"):
				info.Interface = v
				isIface = true
			case keyIs(k, "name", "имя") && info.Interface == "":
				info.Interface = v
				isIface = true
			case keyIs(k, "state", "состояние"):
				lv := strings.ToLower(v)
				info.Connected = (strings.Contains(lv, "connected") && !strings.Contains(lv, "disconnected")) ||
					(strings.Contains(lv, "подключ") && !strings.Contains(lv, "не подключ") && !strings.Contains(lv, "отключ"))
			case keyIs(k, "manufacturer", "производитель", "изготовитель"):
				manu = v
			case keyIs(k, "model", "модель"):
				model = v
			case keyHas(k, "provider name", "имя поставщика", "имя оператора", "поставщик", "оператор"):
				info.Operator = v
			case keyHas(k, "data class", "класс данных"):
				info.Tech = normTech(v)
			case strings.HasSuffix(v, "%") && (keyIs(k, "signal", "сигнал") || keyHas(k, "signal", "сигнал")):
				if n, ok := atoiPrefix(v); ok {
					info.SignalPct = clampPct(n)
				}
			}
		}
		if !isIface {
			continue
		}
		info.Model = joinModel(manu, model)
		out = append(out, info)
	}
	return out
}

// parseMbnMCCMNC: from `netsh mbn show homeprovider` (a 5-6 digit
// provider id) or `readyinfo` (IMSI, 14-15 digits; the ICCID is longer
// and starts with 89).
func parseMbnMCCMNC(text string) string {
	for _, blk := range netshBlocks(text) {
		for _, p := range blk {
			v := strings.TrimSpace(p.val)
			if !isDigits(v) {
				continue
			}
			switch {
			case len(v) >= 5 && len(v) <= 6 && keyHas(p.key, "provider id", "идентификатор поставщика", "id поставщика", "поставщик", "provider"):
				return v
			case len(v) >= 14 && len(v) <= 15 && !strings.HasPrefix(v, "89"):
				return mccmncFromIMSI(v)
			}
		}
	}
	return ""
}

func atoiPrefix(s string) (int, bool) {
	n, got := 0, false
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			break
		}
		n, got = n*10+int(r-'0'), true
	}
	return n, got
}
