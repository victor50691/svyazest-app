// Package modem: what is connected as "the partner's mobile link" and what
// the operator says about itself. Owner: agent "net+packaging".
//
// CONTRACT:
package modem

import "context"

type Info struct {
	Interface string // network interface carrying the data (matches netbind.Iface.Name)
	Operator  string // "MTS RUS", "Beeline", "MegaFon", "T2", ... as reported
	MCCMNC    string // "25001"; "" if unknown
	Country   string // ISO-3166 alpha-2 lower case from MCC ("ru"), "" if unknown
	Tech      string // "lte" | "5gnr" | "umts" | "gsm" | "" ...
	SignalPct int    // 0..100, -1 unknown
	Model     string // "Huawei E3372h", "Quectel EC25", "Android phone (USB tethering)"
	Source    string // "modemmanager" | "hilink" | "mbn" | "tether" | "manual"
	Connected bool   // data bearer up
}

// Detect lists mobile links currently present. Never blocks > ~5 s.
// Linux: ModemManager (mmcli -J / D-Bus), Huawei HiLink HTTP API
// (192.168.8.1, ZTE 192.168.0.1 ...), RNDIS/cdc_ether phone tethering.
// Windows: Mobile Broadband (netsh mbn / WWAN), HiLink, RNDIS tethering.
func Detect(ctx context.Context) ([]Info, error) { return detectImpl(ctx) }
