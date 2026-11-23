package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"svyazest-agent/internal/checks"
)

// DeviceMeta goes with pair and auth/start (executor_devices row).
type DeviceMeta struct {
	Platform   string // linux | windows
	Model      string // "Raspberry Pi 4 Model B Rev 1.4", "ThinkPad T14 (Docker)"
	OSVersion  string // "Debian 13 arm64", "Windows 11 23H2"
	Arch       string // "amd64" | "arm64" | "armv7"
	AppVersion string
	HWID       string // sha256 hex
}

func (m DeviceMeta) body() map[string]any {
	b := map[string]any{
		"device_model":    m.Model,
		"android_version": m.OSVersion, // column name on the server; holds the OS string
		"os_version":      m.OSVersion,
		"platform":        m.Platform,
		"os_arch":         m.Arch,
		"app_version":     m.AppVersion,
	}
	if m.HWID != "" {
		b["hwid"] = m.HWID
	}
	return b
}

type PairResult struct {
	DeviceToken string     `json:"device_token"`
	DeviceID    FlexInt    `json:"device_id"`
	ExecutorID  FlexString `json:"executor_id"`
}

// Pair: one-time code from the bot («Код для входа»).
func (c *Client) Pair(ctx context.Context, code string, m DeviceMeta) (PairResult, error) {
	b := m.body()
	b["code"] = code
	var out PairResult
	err := c.call(ctx, http.MethodPost, "/pair", b, &out, reqOpts{})
	if err == nil && out.DeviceToken == "" {
		err = fmt.Errorf("сервер не вернул токен")
	}
	return out, err
}

type AuthStart struct {
	Code      string  `json:"code"`
	URL       string  `json:"url"`
	ExpiresIn FlexInt `json:"expires_in"`
}

func (c *Client) AuthStart(ctx context.Context, m DeviceMeta) (AuthStart, error) {
	var out AuthStart
	err := c.call(ctx, http.MethodPost, "/auth/start", m.body(), &out, reqOpts{})
	return out, err
}

// AuthPoll: status pending | ok | expired; Pair set for ok.
type AuthPoll struct {
	Status string
	Pair   PairResult
}

func (c *Client) AuthPoll(ctx context.Context, code string) (AuthPoll, error) {
	var out struct {
		Status string `json:"status"`
		PairResult
	}
	err := c.call(ctx, http.MethodGet, "/auth/poll?code="+q(code), nil, &out, reqOpts{})
	if e, ok := AsError(err); ok && e.Status == 404 {
		return AuthPoll{Status: "expired"}, nil
	}
	if err != nil {
		return AuthPoll{}, err
	}
	if out.Status == "" {
		out.Status = "pending"
	}
	if out.Status == "ok" && out.DeviceToken == "" {
		return AuthPoll{Status: "pending"}, nil
	}
	return AuthPoll{Status: out.Status, Pair: out.PairResult}, nil
}

// ---- heartbeat ----

type ModemReport struct {
	Interface string `json:"interface,omitempty"`
	Operator  string `json:"operator,omitempty"`
	MCCMNC    string `json:"mccmnc,omitempty"`
	Country   string `json:"country,omitempty"`
	Tech      string `json:"tech,omitempty"`
	Model     string `json:"model,omitempty"`
	Source    string `json:"source,omitempty"`
	SignalPct int    `json:"signal_pct"`
	Connected bool   `json:"connected"`
}

type Heartbeat struct {
	MobileDataEnabled bool         `json:"mobile_data_enabled"`
	IsOnline          bool         `json:"is_online"`
	OperatorName      string       `json:"operator_name,omitempty"`
	HWID              string       `json:"hwid,omitempty"`
	Country           string       `json:"country,omitempty"`
	SimCountry        string       `json:"sim_country,omitempty"`
	WhitelistVerdict  string       `json:"whitelist_verdict,omitempty"`
	Platform          string       `json:"platform,omitempty"`
	Modem             *ModemReport `json:"modem,omitempty"`
	// CellularIP: the operator's public IP measured over the modem
	// (checks.CellularIP) -- sent when the modem gives no country (MCC).
	CellularIP map[string]any `json:"cellular_ip,omitempty"`
	// IPv6 (1.0.1, Android 1.17.2 heartbeat field "ipv6"): whether the
	// modem's network carries IPv6 right now (re-measured every 10 min);
	// only such devices are given IPv6 jobs. nil = not measured yet.
	IPv6 *bool `json:"ipv6,omitempty"`
}

// Request: support's diag / GPS request ({id, status, reason}).
type Request struct {
	ID     FlexInt `json:"id"`
	Status string  `json:"status"`
	Reason string  `json:"reason"`
}

type HeartbeatReply struct {
	WhitelistAllowed  []string
	WhitelistControl  []string
	WhitelistInterval time.Duration
	WhitelistRequired bool
	Diag              *Request
	Gps               *Request
	AttestDue         bool
	// 1.0.6: what the server made of the mobile network's IP (a phone over
	// USB tells the computer no operator / country): owner of the IP and
	// the country the server goes by.
	NetOperator string
	NetCountry  string
}

func (h HeartbeatReply) CanProbe() bool {
	return len(h.WhitelistAllowed) > 0 && len(h.WhitelistControl) > 0
}

func (c *Client) Heartbeat(ctx context.Context, hb Heartbeat) (HeartbeatReply, error) {
	if hb.Platform == "" {
		hb.Platform = c.Platform
	}
	var out struct {
		WhitelistAllowed  []string `json:"whitelist_allowed"`
		WhitelistControl  []string `json:"whitelist_control"`
		WhitelistInterval FlexInt  `json:"whitelist_interval"`
		WhitelistRequired *bool    `json:"whitelist_required"`
		Diag              *Request `json:"diag"`
		Gps               *Request `json:"gps"`
		AttestDue         bool     `json:"attest_due"`
		Country           string   `json:"country"`
		CellularOperator  string   `json:"cellular_ip_operator"`
	}
	if err := c.call(ctx, http.MethodPost, "/heartbeat", hb, &out, reqOpts{auth: true}); err != nil {
		return HeartbeatReply{}, err
	}
	r := HeartbeatReply{
		WhitelistAllowed:  out.WhitelistAllowed,
		WhitelistControl:  out.WhitelistControl,
		WhitelistInterval: time.Duration(out.WhitelistInterval) * time.Second,
		WhitelistRequired: out.WhitelistRequired == nil || *out.WhitelistRequired,
		Diag:              validReq(out.Diag),
		Gps:               validReq(out.Gps),
		AttestDue:         out.AttestDue,
		NetOperator:       strings.TrimSpace(out.CellularOperator),
		NetCountry:        strings.ToLower(strings.TrimSpace(out.Country)),
	}
	if r.WhitelistInterval <= 0 {
		r.WhitelistInterval = 5 * time.Minute
	}
	return r, nil
}

func validReq(r *Request) *Request {
	if r == nil || r.ID <= 0 {
		return nil
	}
	return r
}

// ---- attestation (desktop) ----

func (c *Client) AttestChallenge(ctx context.Context) (string, error) {
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := c.call(ctx, http.MethodPost, "/attest/challenge", nil, &out, reqOpts{auth: true}); err != nil {
		return "", err
	}
	if out.Challenge == "" {
		return "", fmt.Errorf("сервер не выдал challenge")
	}
	return out.Challenge, nil
}

// AttestDesktop binds the software device key: challenge -> signature
// over the challenge string -> POST /attest/desktop. Returns the status
// ("desktop" on success).
func (c *Client) AttestDesktop(ctx context.Context) (string, error) {
	k := c.Key()
	if k == nil {
		return "", fmt.Errorf("нет ключа устройства")
	}
	ch, err := c.AttestChallenge(ctx)
	if err != nil {
		return "", err
	}
	pub, err := PublicPEM(k)
	if err != nil {
		return "", err
	}
	sig, err := SignString(k, ch)
	if err != nil {
		return "", err
	}
	var out struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	err = c.call(ctx, http.MethodPost, "/attest/desktop",
		map[string]any{"pubkey": pub, "challenge": ch, "signature": sig, "platform": c.Platform},
		&out, reqOpts{auth: true, timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	if out.Status == "" {
		out.Status = "error"
	}
	if out.Reason != "" && out.Status != "desktop" {
		return out.Status + " (" + out.Reason + ")", nil
	}
	return out.Status, nil
}

// ---- jobs ----

type PollResult struct {
	Job                 *checks.Job
	Moderation          string // pending | rejected | "" (approved / not said)
	WhitelistRequired   bool
	AttestationRequired bool
	DesktopDisabled     bool // computers are switched off on the server
	CountryUnknown      bool // country_unknown: no MCC country and no IP country yet
	NeedCellularIP      bool // measure cellular_ip now, send it with the next heartbeat
}

type wireJob struct {
	ID               FlexInt  `json:"id"`
	CheckID          FlexInt  `json:"check_id"`
	Resource         string   `json:"resource"`
	ResourceType     string   `json:"resource_type"`
	Protocol         *string  `json:"protocol"`
	NeedsCore        *string  `json:"needs_core"`
	ProbeSites       []string `json:"probe_sites"`
	DownloadURL      string   `json:"download_url"`
	WhitelistAllowed []string `json:"whitelist_allowed"`
	WhitelistControl []string `json:"whitelist_control"`
	IPFamily         *FlexInt `json:"ip_family"`
}

// PollJob: long poll (server holds ~25 s, more when it idles us).
func (c *Client) PollJob(ctx context.Context) (PollResult, error) {
	var out struct {
		Job                 *wireJob `json:"job"`
		Moderation          string   `json:"moderation"`
		WhitelistRequired   bool     `json:"whitelist_required"`
		AttestationRequired bool     `json:"attestation_required"`
		DesktopDisabled     bool     `json:"desktop_disabled"`
		CountryUnknown      bool     `json:"country_unknown"`
		NeedCellularIP      bool     `json:"need_cellular_ip"`
	}
	if err := c.call(ctx, http.MethodGet, "/jobs/poll", nil, &out, reqOpts{auth: true, timeout: 45 * time.Second}); err != nil {
		return PollResult{}, err
	}
	r := PollResult{Moderation: out.Moderation, WhitelistRequired: out.WhitelistRequired, AttestationRequired: out.AttestationRequired,
		DesktopDisabled: out.DesktopDisabled, CountryUnknown: out.CountryUnknown, NeedCellularIP: out.NeedCellularIP}
	if w := out.Job; w != nil && w.ID > 0 {
		j := checks.Job{
			ID: int64(w.ID), CheckID: int64(w.CheckID), Resource: w.Resource, ResourceType: w.ResourceType,
			ProbeSites: w.ProbeSites, DownloadURL: w.DownloadURL,
			WhitelistAllowed: w.WhitelistAllowed, WhitelistControl: w.WhitelistControl,
		}
		if w.Protocol != nil {
			j.Protocol = *w.Protocol
		}
		if w.NeedsCore != nil {
			j.NeedsCore = *w.NeedsCore
		}
		if w.IPFamily != nil && (*w.IPFamily == 4 || *w.IPFamily == 6) {
			j.IPFamily = int(*w.IPFamily)
		}
		r.Job = &j
	}
	return r, nil
}

func (c *Client) JobAlive(ctx context.Context, id int64) error {
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/alive", id), nil, nil, reqOpts{auth: true, timeout: 4 * time.Second})
}

func (c *Client) ReleaseJob(ctx context.Context, id int64) (bool, error) {
	var out struct {
		Released bool `json:"released"`
	}
	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/release", id), nil, &out, reqOpts{auth: true, timeout: 10 * time.Second})
	return out.Released, err
}

func (c *Client) SubmitResult(ctx context.Context, id int64, success bool, result map[string]any) error {
	status := "failed"
	if success {
		status = "completed"
	}
	if result == nil {
		result = map[string]any{}
	}
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/result", id),
		map[string]any{"status": status, "result": result}, nil, reqOpts{auth: true, timeout: 20 * time.Second})
}

// ---- misc ----

// Log: breadcrumb on the device row (executor_devices.last_error).
func (c *Client) Log(ctx context.Context, message string) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	return c.call(ctx, http.MethodPost, "/log", map[string]any{"message": message}, nil, reqOpts{auth: true, timeout: 10 * time.Second})
}

// NetworkReport over the modem (doer = the modem's http.Client; via =
// "cellular") or over the control channel (doer nil; via = "default").
func (c *Client) NetworkReport(ctx context.Context, doer Doer, body map[string]any) error {
	b := map[string]any{}
	for k, v := range body {
		b[k] = v
	}
	if doer != nil {
		b["via"] = "cellular"
	} else {
		b["via"] = "default"
	}
	return c.call(ctx, http.MethodPost, "/network-report", b, nil, reqOpts{auth: true, timeout: 15 * time.Second, doer: doer})
}

// Profile: decoded fields the agent needs + the raw object for the UI.
type Profile struct {
	ID                FlexString     `json:"id"`
	Balance           FlexFloat      `json:"balance"`
	ModerationStatus  string         `json:"moderation_status"`
	ModerationReason  string         `json:"moderation_reason"`
	Country           string         `json:"country"`
	CountryAllowed    *bool          `json:"country_allowed"`
	AppOutdated       bool           `json:"app_outdated"`
	MinAppVersion     string         `json:"min_app_version"`
	UpdateURL         string         `json:"update_url"`
	AttestationStatus string         `json:"attestation_status"`
	Diag              *Request       `json:"diag"`
	Gps               *Request       `json:"gps"`
	Raw               map[string]any `json:"-"`
}

func (c *Client) Profile(ctx context.Context) (Profile, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, "/profile", nil, &raw, reqOpts{auth: true}); err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return Profile{}, err
	}
	_ = json.Unmarshal(raw, &p.Raw)
	p.Diag, p.Gps = validReq(p.Diag), validReq(p.Gps)
	if p.ModerationStatus == "" {
		p.ModerationStatus = "approved"
	}
	return p, nil
}

type HistoryJob struct {
	ID           FlexInt    `json:"id"`
	ResourceType string     `json:"resource_type"`
	Protocol     *string    `json:"protocol"`
	Status       string     `json:"status"`
	RewardAmount FlexFloat  `json:"reward_amount"`
	CreatedAt    *time.Time `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at"`
}

func (c *Client) JobHistory(ctx context.Context) ([]HistoryJob, error) {
	var out struct {
		Jobs []HistoryJob `json:"jobs"`
	}
	err := c.call(ctx, http.MethodGet, "/jobs/history", nil, &out, reqOpts{auth: true})
	return out.Jobs, err
}

// ---- diagnostics logs (with consent) ----

type DiagLine struct {
	T int64  `json:"t"`
	S string `json:"s"`
	M string `json:"m"`
}

func (c *Client) DiagDecision(ctx context.Context, id int64, accept bool) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/diag/%d/decision", id), map[string]any{"accept": accept}, &out, reqOpts{auth: true})
	return out.Status, err
}

func (c *Client) DiagStop(ctx context.Context, id int64) error {
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/diag/%d/stop", id), nil, nil, reqOpts{auth: true})
}

// DiagLogs: false = the request is no longer accepted, stop collecting.
func (c *Client) DiagLogs(ctx context.Context, id int64, lines []DiagLine) (bool, error) {
	var out struct {
		Active *bool `json:"active"`
	}
	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/diag/%d/logs", id), map[string]any{"lines": lines}, &out,
		reqOpts{auth: true, timeout: 20 * time.Second})
	if err != nil {
		return true, err
	}
	return out.Active == nil || *out.Active, nil
}

// ---- GPS (a computer has none) ----

func (c *Client) GpsDecision(ctx context.Context, id int64, accept bool) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/gps/%d/decision", id), map[string]any{"accept": accept}, &out, reqOpts{auth: true})
	return out.Status, err
}

func (c *Client) GpsResult(ctx context.Context, id int64, data map[string]any) error {
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/gps/%d/result", id), data, nil, reqOpts{auth: true, timeout: 20 * time.Second})
}
