//go:build linux

package modem

import "testing"

func TestMMModem(t *testing.T) {
	js := `{"modem":{"3gpp":{"imei":"8613","operator-code":"25001","operator-name":"MTS RUS","registration-state":"home"},"generic":{"access-technologies":["lte"],"bearers":["/org/freedesktop/ModemManager1/Bearer/0"],"drivers":["qmi_wwan","option1"],"manufacturer":"Quectel","model":"EC25","ports":["cdc-wdm0 (qmi)","ttyUSB2 (at)","wwan0 (net)"],"signal-quality":{"recent":"yes","value":"67"},"state":"connected"}}}`
	i, b, ok := parseMMModem([]byte(js))
	if !ok || i.Interface != "wwan0" || i.Operator != "MTS RUS" || i.MCCMNC != "25001" || i.Country != "ru" || i.Tech != "lte" || i.SignalPct != 67 || i.Model != "Quectel EC25" || !i.Connected || len(b) != 1 {
		t.Fatalf("%+v %v", i, b)
	}
	ifn, c := parseMMBearer([]byte(`{"bearer":{"status":{"connected":"yes","interface":"wwan0"}}}`))
	if ifn != "wwan0" || !c {
		t.Fatal(ifn, c)
	}
	j2 := `{"modem":{"3gpp":{"operator-code":"--","operator-name":"--"},"generic":{"access-technologies":[],"manufacturer":"--","model":"--","signal-quality":{"value":"0"},"state":"disabled"}}}`
	i2, _, _ := parseMMModem([]byte(j2))
	if i2.Operator != "" || i2.MCCMNC != "" || i2.Connected || i2.SignalPct != 0 {
		t.Fatalf("%+v", i2)
	}
}
