//go:build linux

package netbind

import (
	"fmt"
	"testing"
)

func TestParseResolvectl(t *testing.T) {
	out := "Global: 1.1.1.1\nLink 2 (eth0): 192.168.1.1 fe80::1%eth0\nLink 5 (wwan0): 10.0.0.1#dns.example 2a00:1::1\nLink 7 (docker0):\n"
	m := parseResolvectl([]byte(out))
	// The zone stays: a link-local resolver is only reachable with it.
	if fmt.Sprint(m["eth0"]) != "[192.168.1.1 fe80::1%eth0]" {
		t.Fatalf("eth0 %v", m["eth0"])
	}
	if fmt.Sprint(m["wwan0"]) != "[10.0.0.1 2a00:1::1]" {
		t.Fatalf("wwan0 %v", m["wwan0"])
	}
	if len(m["docker0"]) != 0 {
		t.Fatal(m["docker0"])
	}
}

func TestParseNmcli(t *testing.T) {
	out := "GENERAL.DEVICE:wlan0\nIP4.DNS[1]:192.168.0.1\nIP6.DNS[1]:2a02\\:6b8\\:\\:feed\\:ff\nGENERAL.DEVICE:lo\n"
	m := parseNmcli([]byte(out))
	if fmt.Sprint(m["wlan0"]) != "[192.168.0.1 2a02:6b8::feed:ff]" {
		t.Fatalf("%v", m)
	}
}

func TestParseDhclient(t *testing.T) {
	in := `lease {
  interface "eth0";
  option domain-name-servers 10.1.1.1,10.1.1.2;
}
lease {
  interface "eth0";
  option domain-name-servers 10.2.2.2;
}
lease {
  interface "usb0";
  option domain-name-servers 192.168.42.129;
}`
	m := parseDhclientLeases([]byte(in))
	if fmt.Sprint(m["eth0"]) != "[10.2.2.2]" || fmt.Sprint(m["usb0"]) != "[192.168.42.129]" {
		t.Fatalf("%v", m)
	}
}

func TestParseDhcpcd(t *testing.T) {
	out := "broadcast_address='192.168.1.255'\ndomain_name_servers='192.168.1.1 8.8.8.8'\n"
	if a := parseDhcpcdDump([]byte(out)); fmt.Sprint(a) != "[192.168.1.1 8.8.8.8]" {
		t.Fatal(a)
	}
}
