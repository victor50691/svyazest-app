package qr

import (
	"os"
	"strings"
	"testing"
)

// TestDump writes matrices for cross-checking against an independent
// encoder (python qrcode) when QR_DUMP is set: "version mask rows...".
func TestDump(t *testing.T) {
	out := os.Getenv("QR_DUMP")
	if out == "" {
		t.Skip("QR_DUMP not set")
	}
	texts := []string{
		"https://t.me/svyaz_estt_bot?start=app_0123456789abcdef0123456789abcdef0123456789abcdef",
		"hello",
		strings.Repeat("Связь Есть? ", 20),
	}
	var b strings.Builder
	for ti, tx := range texts {
		for m := 0; m < 8; m++ {
			c, err := EncodeMask(tx, m, 0)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(string(rune('0'+ti)) + " " + itoa(c.Version) + " " + itoa(m) + "\n")
			for y := 0; y < c.Size; y++ {
				for x := 0; x < c.Size; x++ {
					if c.Modules[y][x] {
						b.WriteByte('1')
					} else {
						b.WriteByte('0')
					}
				}
				b.WriteByte('\n')
			}
		}
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	s := ""
	for v > 0 {
		s = string(rune('0'+v%10)) + s
		v /= 10
	}
	return s
}

func TestEncode(t *testing.T) {
	c, err := Encode("https://t.me/svyaz_estt_bot?start=app_0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 6 {
		t.Fatalf("version %d", c.Version)
	}
	if !strings.Contains(c.SVG("#000", "#fff"), "<svg") || len(c.Terminal()) == 0 {
		t.Fatal("render")
	}
}
