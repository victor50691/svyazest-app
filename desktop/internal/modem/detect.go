package modem

// Detect = what the OS's modem stack says (ModemManager on Linux, Mobile
// Broadband on Windows) + router-style sticks asked over HTTP through
// their own interface (Huawei HiLink, ZTE) + whatever else looks like a
// USB-tethered phone. Everything runs in parallel under one deadline: the
// UI calls this on every refresh and must never hang on a dead stick.

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"svyazest-agent/internal/netbind"
)

const detectBudget = 4500 * time.Millisecond

// Router-style modems answer on their gateway address.
var zteGateways = map[netip.Addr]bool{
	netip.MustParseAddr("192.168.0.1"):  true,
	netip.MustParseAddr("192.168.32.1"): true,
	netip.MustParseAddr("192.168.1.1"):  true,
}
var hilinkGW = netip.MustParseAddr("192.168.8.1")

func detectImpl(ctx context.Context) ([]Info, error) {
	ctx, cancel := context.WithTimeout(ctx, detectBudget)
	defer cancel()

	ifs, _ := netbind.ListDetailed()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		platform []Info
		routers  = map[string]Info{}
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		p := platformDetect(ctx)
		mu.Lock()
		platform = p
		mu.Unlock()
	}()
	for _, d := range ifs {
		if !d.Up || !d.Gateway.Is4() || !d.Gateway.IsPrivate() {
			continue
		}
		hilinkCand := d.Kind == "modem" || d.Gateway == hilinkGW
		zteCand := d.Kind == "modem" && zteGateways[d.Gateway]
		if !hilinkCand && !zteCand {
			continue
		}
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			if info, ok := probeRouter(ctx, d, hilinkCand, zteCand); ok {
				mu.Lock()
				routers[d.Name] = info
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	var out []Info
	seen := map[string]bool{}
	for _, i := range platform {
		out = append(out, i)
		if i.Interface != "" {
			seen[i.Interface] = true
		}
	}
	names := make([]string, 0, len(routers))
	for n := range routers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !seen[n] {
			out = append(out, routers[n])
			seen[n] = true
		}
	}
	// Modem-looking interfaces nobody described: a phone sharing its
	// connection over USB (it tells us nothing about the operator), or a
	// stick in network mode without ModemManager running.
	for _, d := range ifs {
		if d.Kind != "modem" || seen[d.Name] {
			continue
		}
		out = append(out, tetherInfo(d))
		seen[d.Name] = true
	}
	return out, nil
}

func tetherInfo(d netbind.IfaceDetail) Info {
	model := "Телефон или модем по USB"
	drv := strings.ToLower(d.Driver)
	desc := strings.ToLower(d.Description)
	switch {
	case drv == "ipheth" || strings.Contains(desc, "apple") || strings.Contains(desc, "iphone"):
		model = "iPhone (USB-модем)"
	case drv == "rndis_host" || drv == "cdc_ncm" || strings.Contains(desc, "remote ndis") || strings.Contains(desc, "rndis") || strings.Contains(desc, "android"):
		model = "Телефон Android (USB-модем)"
	case d.Description != "":
		model = d.Description
	}
	return Info{
		Interface: d.Name,
		Model:     model,
		Source:    "tether",
		SignalPct: -1,
		Connected: d.Up && (d.Gateway.IsValid() || d.DefaultRoute),
	}
}

// normTech: a technology name as reported -> "gsm"|"umts"|"lte"|"5gnr".
func normTech(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "5g") || strings.Contains(s, "nr"):
		return "5gnr"
	case strings.Contains(s, "lte") || strings.Contains(s, "4g"):
		return "lte"
	case strings.Contains(s, "umts") || strings.Contains(s, "hspa") || strings.Contains(s, "hsdpa") ||
		strings.Contains(s, "hsupa") || strings.Contains(s, "wcdma") || strings.Contains(s, "3g") ||
		strings.Contains(s, "td-scdma") || strings.Contains(s, "tdscdma") || strings.Contains(s, "evdo"):
		return "umts"
	case strings.Contains(s, "gsm") || strings.Contains(s, "gprs") || strings.Contains(s, "edge") || strings.Contains(s, "2g"):
		return "gsm"
	}
	return s
}

// bestTech: the newest generation in a list of technology names.
var techRank = map[string]int{"gsm": 1, "umts": 2, "lte": 3, "5gnr": 4}

func bestTech(list []string) string {
	best := ""
	for _, t := range list {
		n := normTech(t)
		if techRank[n] > techRank[best] || best == "" {
			best = n
		}
	}
	return best
}

func clampPct(v int) int {
	if v < 0 {
		return -1
	}
	if v > 100 {
		return 100
	}
	return v
}

// joinModel: "QUALCOMM INCORPORATED" + "QUECTEL Mobile Broadband Module"
// stays as is; "Huawei" + "Huawei E3372" is not doubled.
func joinModel(manu, model string) string {
	if manu == "" {
		return model
	}
	f := strings.Fields(manu)
	if model == "" || len(f) == 0 {
		return strings.TrimSpace(manu + " " + model)
	}
	if strings.Contains(strings.ToLower(model), strings.ToLower(f[0])) {
		return model
	}
	return manu + " " + model
}
