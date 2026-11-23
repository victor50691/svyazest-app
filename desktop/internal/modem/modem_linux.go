//go:build linux

package modem

// Linux: ModemManager through its own CLI (mmcli -J gives stable JSON
// since MM 1.12, present on Debian/Raspberry Pi OS with the modemmanager
// package; in Docker it talks to the host's MM over the mounted D-Bus
// socket). Without mmcli this simply finds nothing and Detect falls back
// to HiLink/ZTE/tethering.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func platformDetect(ctx context.Context) []Info {
	out, err := run(ctx, 2*time.Second, "mmcli", "-L", "-J")
	if err != nil {
		return nil
	}
	var list struct {
		Modems []string `json:"modem-list"`
	}
	if json.Unmarshal(out, &list) != nil {
		return nil
	}
	var res []Info
	for _, p := range list.Modems {
		mo, err := run(ctx, 2*time.Second, "mmcli", "-m", lastElem(p), "-J")
		if err != nil {
			continue
		}
		info, bearers, ok := parseMMModem(mo)
		if !ok {
			continue
		}
		// The data interface is the bearer's (the one carrying the
		// connection now); the "(net)" port is the fallback.
		for _, b := range bearers {
			bo, err := run(ctx, 2*time.Second, "mmcli", "-b", lastElem(b), "-J")
			if err != nil {
				continue
			}
			if ifn, conn := parseMMBearer(bo); ifn != "" {
				if conn || info.Interface == "" {
					info.Interface = ifn
				}
				if conn {
					info.Connected = true
					break
				}
			}
		}
		res = append(res, info)
	}
	return res
}

// mmcli -J fields we use. Values are strings; "--" means unknown.
type mmModem struct {
	Modem struct {
		ThreeGPP struct {
			OperatorCode string `json:"operator-code"`
			OperatorName string `json:"operator-name"`
		} `json:"3gpp"`
		Generic struct {
			AccessTechnologies []string `json:"access-technologies"`
			Bearers            []string `json:"bearers"`
			Manufacturer       string   `json:"manufacturer"`
			Model              string   `json:"model"`
			Ports              []string `json:"ports"`
			State              string   `json:"state"`
			SignalQuality      struct {
				Value string `json:"value"`
			} `json:"signal-quality"`
		} `json:"generic"`
	} `json:"modem"`
}

func parseMMModem(b []byte) (Info, []string, bool) {
	var m mmModem
	if json.Unmarshal(b, &m) != nil {
		return Info{}, nil, false
	}
	g, t := m.Modem.Generic, m.Modem.ThreeGPP
	info := Info{Source: "modemmanager", SignalPct: -1}
	info.Operator = mmVal(t.OperatorName)
	if c := mmVal(t.OperatorCode); len(c) >= 5 && isDigits(c) {
		info.MCCMNC = c
		info.Country = CountryByMCC(c[:3])
	}
	info.Tech = bestTech(g.AccessTechnologies)
	if v, err := strconv.Atoi(mmVal(g.SignalQuality.Value)); err == nil {
		info.SignalPct = clampPct(v)
	}
	info.Model = joinModel(mmVal(g.Manufacturer), mmVal(g.Model))
	info.Connected = mmVal(g.State) == "connected"
	for _, p := range g.Ports {
		// "wwan0 (net)"
		if name, kind, ok := strings.Cut(p, " "); ok && strings.Contains(kind, "(net)") {
			info.Interface = name
			break
		}
	}
	return info, g.Bearers, true
}

func parseMMBearer(b []byte) (iface string, connected bool) {
	var br struct {
		Bearer struct {
			Status struct {
				Connected string `json:"connected"`
				Interface string `json:"interface"`
			} `json:"status"`
		} `json:"bearer"`
	}
	if json.Unmarshal(b, &br) != nil {
		return "", false
	}
	return mmVal(br.Bearer.Status.Interface), br.Bearer.Status.Connected == "yes"
}

func mmVal(s string) string {
	s = strings.TrimSpace(s)
	if s == "--" || s == "unknown" {
		return ""
	}
	return s
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Output()
}

func lastElem(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
