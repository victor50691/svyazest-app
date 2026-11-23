package modem

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMbnEnglish(t *testing.T) {
	out := "\r\nThere is 1 interface on the system:\r\n\r\n    Name               : Cellular\r\n    Description        : Generic Mobile Broadband Adapter\r\n    GUID               : {1234-5678}\r\n    Physical Address   : 00:a0:c6:00:00:00\r\n    State              : Connected\r\n    Device type        : Mobile Broadband device is embedded in the system\r\n    Cellular class     : GSM\r\n    Device Id          : 359072061234567\r\n    Manufacturer       : Quectel\r\n    Model              : EM05-G\r\n    Firmware Version   : EM05GFAR07A07M1G\r\n    Provider Name      : MTS RUS\r\n    Roaming            : Not roaming\r\n    Signal             : 74%\r\n    RSSI / RSCP        : 20 (-73 dBm)\r\n"
	l := parseMbnInterfaces(out)
	if len(l) != 1 {
		t.Fatalf("%+v", l)
	}
	i := l[0]
	if i.Interface != "Cellular" || !i.Connected || i.Operator != "MTS RUS" || i.SignalPct != 74 || i.Model != "Quectel EM05-G" || i.Source != "mbn" {
		t.Fatalf("%+v", i)
	}
}

// Russian Windows in CP866, as netsh writes into a pipe.
func TestMbnRussianCP866(t *testing.T) {
	utf := "    Имя                : Сотовая связь\n    Состояние          : Подключено\n    Производитель      : Huawei\n    Модель             : ME906s\n    Имя поставщика     : MegaFon\n    Сигнал             : 55%\n"
	l := parseMbnInterfaces(decodeOEM(toCP866(utf)))
	if len(l) != 1 || l[0].Interface != "Сотовая связь" || !l[0].Connected || l[0].Operator != "MegaFon" || l[0].SignalPct != 55 || l[0].Model != "Huawei ME906s" {
		t.Fatalf("%+v", l)
	}
	dis := parseMbnInterfaces("    Имя : X\n    Состояние : Не подключено\n")
	if len(dis) != 1 || dis[0].Connected {
		t.Fatalf("%+v", dis)
	}
}

func toCP866(s string) []byte {
	var b []byte
	for _, r := range s {
		switch {
		case r < 0x80:
			b = append(b, byte(r))
		case r >= 0x0410 && r <= 0x043F:
			b = append(b, byte(0x80+r-0x0410))
		case r >= 0x0440 && r <= 0x044F:
			b = append(b, byte(0xE0+r-0x0440))
		}
	}
	return b
}

func TestMbnMCCMNC(t *testing.T) {
	if v := parseMbnMCCMNC("    Home provider ID   : 25002\n    Home provider name : MegaFon\n"); v != "25002" {
		t.Fatal(v)
	}
	if v := parseMbnMCCMNC("    Ready State : Ready\n    Subscriber Id : 250011234567890\n    SIM ICC Id : 8970101234567890123\n"); v != "25001" {
		t.Fatal(v)
	}
	if v := mccmncFromIMSI("310260123456789"); v != "310260" {
		t.Fatal(v)
	}
}

func TestHiLink(t *testing.T) {
	plmn := xmlFields([]byte(`<?xml version="1.0" encoding="UTF-8"?><response><State>0</State><FullName>MTS RUS</FullName><ShortName>MTS</ShortName><Numeric>25001</Numeric><Rat>7</Rat></response>`))
	st := xmlFields([]byte(`<response><ConnectionStatus>901</ConnectionStatus><SignalIcon>4</SignalIcon><CurrentNetworkType>19</CurrentNetworkType><CurrentNetworkTypeEx>1011</CurrentNetworkTypeEx><maxsignal>5</maxsignal></response>`))
	dev := xmlFields([]byte(`<error><code>100003</code><message></message></error>`))
	i := hilinkInfo(plmn, st, dev)
	if i.Operator != "MTS RUS" || i.MCCMNC != "25001" || i.Country != "ru" || i.Tech != "lte" || i.SignalPct != 80 || !i.Connected || i.Model != "Huawei HiLink" {
		t.Fatalf("%+v", i)
	}
	dev2 := xmlFields([]byte(`<response><DeviceName>E3372h-320</DeviceName></response>`))
	if hilinkInfo(plmn, nil, dev2).Model != "Huawei E3372h-320" {
		t.Fatal("model")
	}
	if hilinkNetType("41") != "umts" || hilinkNetType("3") != "gsm" || hilinkNetType("111") != "5gnr" {
		t.Fatal("nettype")
	}
}

func TestZTE(t *testing.T) {
	i, ok := zteInfo(map[string]any{"network_provider": "Beeline", "network_type": "LTE", "signalbar": "3", "ppp_status": "ppp_connected", "modem_model": "MF79U", "rmcc": "250", "rmnc": "99"})
	if !ok || i.Operator != "Beeline" || i.MCCMNC != "25099" || i.Country != "ru" || i.Tech != "lte" || i.SignalPct != 60 || !i.Connected || i.Model != "ZTE MF79U" {
		t.Fatalf("%+v", i)
	}
}

func TestMCC(t *testing.T) {
	for mcc, c := range map[string]string{"250": "ru", "255": "ua", "257": "by", "401": "kz", "434": "uz", "436": "tj", "437": "kg", "438": "tm", "282": "ge", "283": "am", "400": "az", "259": "md", "246": "lt", "247": "lv", "248": "ee", "260": "pl", "262": "de", "244": "fi", "240": "se", "234": "gb", "235": "gb", "208": "fr", "310": "us", "316": "us"} {
		if CountryByMCC(mcc) != c {
			t.Fatal(mcc)
		}
	}
}

func TestTech(t *testing.T) {
	if bestTech([]string{"umts", "lte"}) != "lte" || bestTech([]string{"hspa-plus"}) != "umts" || bestTech([]string{"edge"}) != "gsm" || bestTech([]string{"5gnr", "lte"}) != "5gnr" {
		t.Fatal("tech")
	}
}

// Live: on the build box there is no modem -- Detect must return fast and empty.
func TestLiveDetect(t *testing.T) {
	if os.Getenv("NETBIND_LIVE") == "" {
		t.Skip()
	}
	t0 := time.Now()
	l, err := Detect(context.Background())
	t.Logf("%v %+v %v", time.Since(t0), l, err)
	if time.Since(t0) > 5*time.Second {
		t.Fatal("too slow")
	}
}

// A fake HiLink web server: token per request, login-protected device info.
func TestHiLinkHTTP(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webserver/SesTokInfo":
			n++
			fmt.Fprintf(w, "<response><SesInfo>SessionID=abc%d</SesInfo><TokInfo>tok%d</TokInfo></response>", n, n)
			return
		}
		if r.Header.Get("__RequestVerificationToken") != fmt.Sprintf("tok%d", n) || r.Header.Get("Cookie") != fmt.Sprintf("SessionID=abc%d", n) {
			fmt.Fprint(w, "<error><code>125002</code></error>")
			return
		}
		switch r.URL.Path {
		case "/api/net/current-plmn":
			fmt.Fprint(w, "<response><FullName>Beeline</FullName><Numeric>25099</Numeric><Rat>2</Rat></response>")
		case "/api/monitoring/status":
			fmt.Fprint(w, "<response><ConnectionStatus>901</ConnectionStatus><SignalIcon>5</SignalIcon><CurrentNetworkType>45</CurrentNetworkType></response>")
		default:
			fmt.Fprint(w, "<error><code>100003</code></error>")
		}
	}))
	defer srv.Close()
	i, ok := probeHiLink(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"))
	if !ok || i.Operator != "Beeline" || i.MCCMNC != "25099" || i.Tech != "umts" || i.SignalPct != 100 || !i.Connected {
		t.Fatalf("%v %+v", ok, i)
	}
}
