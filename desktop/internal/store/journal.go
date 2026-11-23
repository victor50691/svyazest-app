package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Journal: the service's own log (data dir journal.log, rotated at 1 MB,
// plus the last lines in memory for the web UI). Every line is masked:
// job targets registered with Secret never reach the disk, the UI or the
// diagnostics upload (Android DiagLog.secret / _hideResource).
//
// Info  -> journal + console + diagnostics (when the partner allowed it)
// Debug -> diagnostics only (and the journal with Verbose)
type Journal struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	ring    []Line
	secrets []string
	diag    func(t time.Time, src, msg string)
	Console io.Writer // nil = quiet
	Verbose bool
}

type Line struct {
	T     time.Time `json:"t"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

const (
	ringMax     = 600
	journalMax  = 1 << 20
	secretsMax  = 300
	maskReplace = "‹цель›"
)

func OpenJournal(dir string) *Journal {
	j := &Journal{path: filepath.Join(dir, "journal.log")}
	if f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		j.f = f
		if fi, err := f.Stat(); err == nil {
			j.size = fi.Size()
		}
	}
	// Last lines of the previous run, so the UI is not empty after a restart.
	if b, err := os.ReadFile(j.path); err == nil {
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > 200 {
			lines = lines[len(lines)-200:]
		}
		for _, l := range lines {
			if ln, ok := parseLine(l); ok {
				j.ring = append(j.ring, ln)
			}
		}
	}
	return j
}

func parseLine(l string) (Line, bool) {
	// "2006-01-02T15:04:05.000Z07:00 LEVEL msg"
	parts := strings.SplitN(l, " ", 3)
	if len(parts) < 3 {
		return Line{}, false
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", parts[0])
	if err != nil {
		return Line{}, false
	}
	return Line{T: t, Level: strings.ToLower(parts[1]), Msg: parts[2]}, true
}

// SetDiagSink: where Info/Debug lines go while log collection is on.
func (j *Journal) SetDiagSink(f func(t time.Time, src, msg string)) {
	j.mu.Lock()
	j.diag = f
	j.mu.Unlock()
}

// Secret registers a string that must never be written anywhere.
func (j *Journal) Secret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.secrets {
		if x == s {
			return
		}
	}
	j.secrets = append(j.secrets, s)
	if len(j.secrets) > secretsMax {
		j.secrets = j.secrets[len(j.secrets)-secretsMax:]
	}
}

func (j *Journal) Mask(s string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.maskLocked(s)
}

func (j *Journal) maskLocked(s string) string {
	// Longest first, so a key that contains its host is replaced whole.
	for pass := 0; pass < 2; pass++ {
		for _, x := range j.secrets {
			if (pass == 0) == (len(x) >= 24) && strings.Contains(s, x) {
				s = strings.ReplaceAll(s, x, maskReplace)
			}
		}
	}
	return s
}

func (j *Journal) Info(format string, a ...any)  { j.write("info", fmt.Sprintf(format, a...), true) }
func (j *Journal) Warn(format string, a ...any)  { j.write("warn", fmt.Sprintf(format, a...), true) }
func (j *Journal) Debug(format string, a ...any) { j.write("debug", fmt.Sprintf(format, a...), false) }

// DebugLine: same as Debug for an already formatted line (checks.Env.Log).
func (j *Journal) DebugLine(msg string) { j.write("debug", msg, false) }

func (j *Journal) write(level, msg string, journal bool) {
	now := time.Now()
	j.mu.Lock()
	msg = j.maskLocked(msg)
	diag := j.diag
	if journal || j.Verbose {
		ln := Line{T: now, Level: level, Msg: msg}
		j.ring = append(j.ring, ln)
		if len(j.ring) > ringMax {
			j.ring = j.ring[len(j.ring)-ringMax:]
		}
		text := now.Format("2006-01-02T15:04:05.000Z07:00") + " " + strings.ToUpper(level) + " " +
			strings.ReplaceAll(msg, "\n", " ⏎ ") + "\n"
		j.appendLocked(text)
		if j.Console != nil {
			fmt.Fprint(j.Console, now.Format("15:04:05")+" "+msg+"\n")
		}
	}
	j.mu.Unlock()
	if diag != nil {
		diag(now, "service", msg)
	}
}

func (j *Journal) appendLocked(text string) {
	if j.f == nil {
		return
	}
	if j.size+int64(len(text)) > journalMax {
		j.f.Close()
		_ = os.Rename(j.path, j.path+".1")
		f, err := os.OpenFile(j.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			j.f = nil
			return
		}
		j.f, j.size = f, 0
	}
	n, _ := j.f.WriteString(text)
	j.size += int64(n)
}

// Tail: the newest n lines (oldest first).
func (j *Journal) Tail(n int) []Line {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n <= 0 || n > len(j.ring) {
		n = len(j.ring)
	}
	return append([]Line(nil), j.ring[len(j.ring)-n:]...)
}

func (j *Journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		j.f.Close()
		j.f = nil
	}
}
