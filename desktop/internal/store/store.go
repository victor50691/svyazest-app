// Package store: everything the agent keeps on disk, in one data directory
// (Linux /var/lib/svyazest-agent, /data in Docker, Windows
// %ProgramData%\SvyazEst; SVYAZEST_DATA / --data-dir override it).
//
// Secrets (device token, device key) are written through protect(): on
// Linux the file is 0600 in a 0700 directory, on Windows the bytes are
// sealed with DPAPI (machine scope) and the directory gets an ACL for
// SYSTEM and Administrators only.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fileDevice   = "device.json"   // token + executor id (protected)
	fileKey      = "device.key"    // ECDSA P-256 PKCS#8 (protected)
	fileSettings = "settings.json" // plain settings, no secrets but the UI password hash
	fileState    = "state.json"    // small runtime state (diag id, last verdict, ...)
	fileHistory  = "history.json"  // local job history, never the targets
	fileMachine  = "machine-id"    // random id when the OS has none
	fileLocalTok = "local-token"   // CLI <-> running agent (status/logout)
	fileSetup    = "ui-setup-code" // one-time code to set the page password
	fileSessions = "ui-sessions.json"
)

// DefaultDir: SVYAZEST_DATA, else the platform default.
func DefaultDir() string {
	if v := os.Getenv("SVYAZEST_DATA"); v != "" {
		return v
	}
	return platformDefaultDir()
}

// InDocker: /.dockerenv or a container cgroup.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	b, _ := os.ReadFile("/proc/1/cgroup")
	s := string(b)
	for _, k := range []string{"docker", "containerd", "kubepods", "libpod"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// Device is the credential the API knows this install by.
type Device struct {
	Token      string    `json:"device_token"`
	ExecutorID string    `json:"executor_id"`
	DeviceID   int64     `json:"device_id"`
	PairedAt   time.Time `json:"paired_at"`
}

// Settings the partner changes in the web UI / CLI.
type Settings struct {
	// ModemIface: "" = automatic (first connected modem), else the
	// interface name chosen in «Ещё → Модем».
	ModemIface string `json:"modem_iface"`
	// UIListen: "" = default (Linux 0.0.0.0:8780, Windows 127.0.0.1:8780).
	UIListen string `json:"ui_listen"`
	// Autostart of the service with the system (default on).
	Autostart *bool `json:"autostart,omitempty"`
	// Page password: PBKDF2-SHA256.
	UIPassHash string `json:"ui_pass_hash,omitempty"`
	UIPassSalt string `json:"ui_pass_salt,omitempty"`
	UIPassIter int    `json:"ui_pass_iter,omitempty"`
	UIPassSet  string `json:"ui_pass_set_at,omitempty"`
}

func (s Settings) AutostartOn() bool { return s.Autostart == nil || *s.Autostart }

// State: runtime bits that should survive a restart.
type State struct {
	DiagActiveID  int64     `json:"diag_active_id,omitempty"`
	GpsAnswered   []int64   `json:"gps_answered,omitempty"`
	LastVerdict   string    `json:"last_verdict,omitempty"`
	LastVerdictAt time.Time `json:"last_verdict_at,omitempty"`
	AttestStatus  string    `json:"attest_status,omitempty"`
	AttestAt      time.Time `json:"attest_at,omitempty"`
	// JobsTotal (1.0.5): jobs done by this computer over all time;
	// seeded once from the local history (which keeps the last 300).
	JobsTotal       int       `json:"jobs_total,omitempty"`
	JobsTotalSeeded bool      `json:"jobs_total_seeded,omitempty"`
	SignedOutNote   string    `json:"signed_out_note,omitempty"`
	RunningSince    time.Time `json:"running_since,omitempty"`
	CleanStop       bool      `json:"clean_stop,omitempty"`
	LastAliveAt     time.Time `json:"last_alive_at,omitempty"`
	SeenLevel       int       `json:"seen_level,omitempty"`
}

// HistoryEntry: what this agent did, without the resource (like the
// app's history screen: type, outcome, time, reward from the server).
type HistoryEntry struct {
	JobID      int64     `json:"job_id"`
	Type       string    `json:"type"` // vpn_key | ip | domain | subnet
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
	DurationMs int64     `json:"duration_ms"`
	Note       string    `json:"note,omitempty"`
}

type Store struct {
	Dir string

	mu       sync.Mutex
	dev      *Device
	devMod   time.Time
	devSeen  bool
	set      Settings
	setMod   time.Time
	setSeen  bool
	stateMu  sync.Mutex
	state    *State
	histMu   sync.Mutex
	hist     []HistoryEntry
	histLoad bool
}

// Open creates the directory if needed and locks it down.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("каталог данных %s: %w", dir, err)
	}
	secureDir(dir)
	return &Store{Dir: dir}, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

// writeFile: atomic (tmp + rename), mode 0600.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func mtime(path string) (time.Time, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// ---- device credential ----

// Device returns the saved credential; re-read when the file changes
// (the CLI `login`/`logout` writes it while the service runs).
func (s *Store) Device() (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(fileDevice)
	mt, ok := mtime(p)
	if !ok {
		s.dev, s.devSeen = nil, false
		return Device{}, false
	}
	if s.devSeen && mt.Equal(s.devMod) {
		if s.dev == nil {
			return Device{}, false
		}
		return *s.dev, true
	}
	s.devSeen, s.devMod, s.dev = true, mt, nil
	raw, err := os.ReadFile(p)
	if err != nil {
		return Device{}, false
	}
	plain, err := unprotect(raw)
	if err != nil {
		return Device{}, false
	}
	var d Device
	if json.Unmarshal(plain, &d) != nil || d.Token == "" {
		return Device{}, false
	}
	s.dev = &d
	return d, true
}

func (s *Store) Token() string {
	d, ok := s.Device()
	if !ok {
		return ""
	}
	return d.Token
}

func (s *Store) SaveDevice(d Device) error {
	if d.PairedAt.IsZero() {
		d.PairedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(d)
	sealed, err := protect(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devSeen = false
	return writeFile(s.path(fileDevice), sealed)
}

func (s *Store) ClearDevice() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devSeen, s.dev = false, nil
	err := os.Remove(s.path(fileDevice))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ---- settings ----

func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingsLocked()
}

func (s *Store) settingsLocked() Settings {
	p := s.path(fileSettings)
	mt, ok := mtime(p)
	if !ok {
		s.set, s.setSeen = Settings{}, false
		return s.set
	}
	if s.setSeen && mt.Equal(s.setMod) {
		return s.set
	}
	var st Settings
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	s.set, s.setMod, s.setSeen = st, mt, true
	return st
}

func (s *Store) UpdateSettings(fn func(*Settings)) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.settingsLocked()
	fn(&st)
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writeFile(s.path(fileSettings), b); err != nil {
		return st, err
	}
	s.setSeen = false
	return st, nil
}

// ---- state ----

func (s *Store) State() State {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return *s.stateLocked()
}

func (s *Store) stateLocked() *State {
	if s.state == nil {
		st := &State{}
		if b, err := os.ReadFile(s.path(fileState)); err == nil {
			_ = json.Unmarshal(b, st)
		}
		s.state = st
	}
	return s.state
}

func (s *Store) UpdateState(fn func(*State)) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st := s.stateLocked()
	fn(st)
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = writeFile(s.path(fileState), b)
}

// ---- local history ----

const historyMax = 300

func (s *Store) loadHistoryLocked() {
	if s.histLoad {
		return
	}
	s.histLoad = true
	if b, err := os.ReadFile(s.path(fileHistory)); err == nil {
		_ = json.Unmarshal(b, &s.hist)
	}
}

func (s *Store) AddHistory(e HistoryEntry) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.loadHistoryLocked()
	s.hist = append([]HistoryEntry{e}, s.hist...)
	if len(s.hist) > historyMax {
		s.hist = s.hist[:historyMax]
	}
	b, _ := json.Marshal(s.hist)
	_ = writeFile(s.path(fileHistory), b)
}

func (s *Store) History() []HistoryEntry {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.loadHistoryLocked()
	return append([]HistoryEntry(nil), s.hist...)
}

func (s *Store) ClearHistory() {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.hist, s.histLoad = nil, true
	_ = os.Remove(s.path(fileHistory))
}

// ---- machine id fallback ----

// RandomMachineID: a random id kept in the data dir, for hosts without
// /etc/machine-id (Docker without the mount, minimal images).
func (s *Store) RandomMachineID() string {
	p := s.path(fileMachine)
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return string(trimSpace(b))
	}
	id := RandomHex(16)
	_ = writeFile(p, []byte(id+"\n"))
	return id
}

// ---- local control token (CLI -> running agent) ----

// NewLocalToken is written by the running agent at start.
func (s *Store) NewLocalToken() string {
	t := RandomHex(24)
	_ = writeFile(s.path(fileLocalTok), []byte(t+"\n"))
	return t
}

func (s *Store) LocalToken() string {
	b, err := os.ReadFile(s.path(fileLocalTok))
	if err != nil {
		return ""
	}
	return string(trimSpace(b))
}

// ---- page setup code ----

// SetupCode: created on first need, deleted once the password is set.
func (s *Store) SetupCode(create bool) string {
	p := s.path(fileSetup)
	if b, err := os.ReadFile(p); err == nil && len(trimSpace(b)) > 0 {
		return string(trimSpace(b))
	}
	if !create {
		return ""
	}
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	out := make([]byte, 0, 9)
	for i, c := range buf {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, alpha[int(c)%len(alpha)])
	}
	_ = writeFile(p, append(out, '\n'))
	return string(out)
}

func (s *Store) DropSetupCode() { _ = os.Remove(s.path(fileSetup)) }

// Sessions file for the web UI (hashes of session tokens + expiry).
func (s *Store) LoadSessions() map[string]time.Time {
	m := map[string]time.Time{}
	if b, err := os.ReadFile(s.path(fileSessions)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (s *Store) SaveSessions(m map[string]time.Time) {
	b, _ := json.Marshal(m)
	_ = writeFile(s.path(fileSessions), b)
}

// ---- helpers ----

func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func trimSpace(b []byte) []byte {
	return []byte(strings.Trim(string(b), " \t\r\n\x00"))
}
