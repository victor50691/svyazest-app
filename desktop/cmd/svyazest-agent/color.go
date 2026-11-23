package main

// Colours for the terminal (1.0.3): only when stdout is a terminal that
// understands ANSI (not Windows, not TERM=dumb, not NO_COLOR) — logs and
// pipes stay plain text.

import (
	"os"
	"runtime"
	"strings"
)

var colorOn = detectColor()

func detectColor() bool {
	if runtime.GOOS == "windows" || os.Getenv("NO_COLOR") != "" {
		return false
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	return isTTY(os.Stdout)
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func sgr(code, s string) string {
	if !colorOn || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func cBold(s string) string   { return sgr("1", s) }
func cDim(s string) string    { return sgr("2", s) }
func cRed(s string) string    { return sgr("31", s) }
func cGreen(s string) string  { return sgr("32", s) }
func cYellow(s string) string { return sgr("33", s) }
func cCyan(s string) string   { return sgr("36", s) }
func cTitle(s string) string  { return sgr("1;36", s) }
func cOK(s string) string     { return sgr("1;32", s) }
func cBad(s string) string    { return sgr("1;31", s) }
func cWarn(s string) string   { return sgr("1;33", s) }

// toneColor: runner.Status.Tone (ok | warn | bad | idle) -> colour.
func toneColor(tone, s string) string {
	switch tone {
	case "ok":
		return cOK(s)
	case "warn":
		return cWarn(s)
	case "bad":
		return cBad(s)
	}
	return cBold(s)
}

// colorUsage: commands green, flags cyan, section titles bold.
func colorUsage(text string) string {
	if !colorOn {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		switch {
		case i == 0:
			lines[i] = cTitle(ln)
		case strings.HasSuffix(ln, ":") && !strings.HasPrefix(ln, " "):
			lines[i] = cBold(ln)
		case strings.HasPrefix(ln, "  svyazest-agent"), strings.HasPrefix(ln, "  --"):
			body := ln[2:]
			cut := strings.Index(body, "  ")
			if cut < 0 {
				cut = len(body)
			}
			col := cGreen
			if strings.HasPrefix(body, "--") {
				col = cCyan
			}
			lines[i] = "  " + col(body[:cut]) + body[cut:]
		}
	}
	return strings.Join(lines, "\n")
}
