package checks

// Share link -> outbound, compared with what vpn_uri_parser.dart +
// build_xray_config.dart produce for the same link (expected values worked
// out by hand from the Dart code, comments say which rule applies).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&x); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(x)
	return string(out)
}

func canonStr(t *testing.T, s string) string {
	t.Helper()
	var x any
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if err := d.Decode(&x); err != nil {
		t.Fatalf("bad expected JSON: %v\n%s", err, s)
	}
	out, _ := json.Marshal(x)
	return string(out)
}

const uid = "11111111-2222-3333-4444-555555555555"

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func b64std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestOutbounds(t *testing.T) {
	extra := `{"xmux":{"cMaxLifetimeMs":0.0,"maxConcurrency":"16-32","hKeepAlivePeriod":1e1},"xPaddingBytes":"100-1000","scMaxEachPostBytes":1000000,"f":1.5}`
	vmessWS := `{"v":"2","ps":"имя","add":"vm.example","port":"8080","id":"` + uid + `","aid":0,"scy":"","net":"ws","type":"none","host":"wsh.example","path":"/vm","tls":"tls","sni":"","alpn":"","fp":"chrome"}`
	vmessGRPC := `{"add":"g.example","port":443.0,"id":"` + uid + `","aid":"2","scy":"aes-128-gcm","net":"grpc","path":"svc","tls":"none"}`
	fm := `{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"100-200"}}]}`
	cases := []struct {
		name, uri, want string
	}{
		{"vless reality vision", "vless://" + uid + "@Example.COM:8443?type=tcp&security=reality&sni=www.microsoft.com&fp=firefox&pbk=PUBKEY123&sid=ab12&flow=xtls-rprx-vision&spx=%2Fpath#My%20Key",
			`{"protocol":"vless","settings":{"vnext":[{"address":"example.com","port":8443,"users":[{"id":"` + uid + `","encryption":"none","flow":"xtls-rprx-vision"}]}]},
			  "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"www.microsoft.com","fingerprint":"firefox","publicKey":"PUBKEY123","shortId":"ab12","spiderX":"/path"}}}`},
		// reality defaults: sni -> address, fp -> chrome, spx -> "/", pbk/sid -> ""; pqv -> mldsa65Verify
		{"vless reality defaults", "vless://" + uid + "@r.example?security=reality&pqv=PQV",
			`{"protocol":"vless","settings":{"vnext":[{"address":"r.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"r.example","fingerprint":"chrome","publicKey":"","shortId":"","spiderX":"/","mldsa65Verify":"PQV"}}}`},
		// tls: sni empty -> host param; alpn split; ech; encryption overrides "none"; no fp key when absent
		{"vless ws tls", "vless://" + uid + "@1.2.3.4?type=ws&security=tls&host=cdn.example.org&path=%2Fws%3Fed%3D2048&alpn=h2,http/1.1&encryption=mlkem768x25519plus.native.0rtt.abc&ech=AEX",
			`{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"` + uid + `","encryption":"mlkem768x25519plus.native.0rtt.abc"}]}]},
			  "streamSettings":{"network":"ws","wsSettings":{"path":"/ws?ed=2048","headers":{"Host":"cdn.example.org"}},"security":"tls","tlsSettings":{"serverName":"cdn.example.org","alpn":["h2","http/1.1"],"echConfigList":"AEX"}}}`},
		// xhttp: host -> address, whole-number doubles -> ints, 1.5 stays, mux honored
		{"vless xhttp extra mux", "vless://" + uid + "@h.example:443?type=xhttp&security=tls&sni=s.example&mode=packet-up&fp=chrome&mux=1&muxConcurrency=4&extra=" + url.QueryEscape(extra),
			`{"protocol":"vless","mux":{"enabled":true,"concurrency":4},"settings":{"vnext":[{"address":"h.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"xhttp","xhttpSettings":{"path":"/","host":"h.example","mode":"packet-up","extra":{"xmux":{"cMaxLifetimeMs":0,"maxConcurrency":"16-32","hKeepAlivePeriod":10},"xPaddingBytes":"100-1000","scMaxEachPostBytes":1000000,"f":1.5}},
			  "security":"tls","tlsSettings":{"serverName":"s.example","fingerprint":"chrome"}}}`},
		{"vless xhttp bad extra ignored", "vless://" + uid + "@h.example?type=xhttp&extra=%7Bnot-json&mode=&mux=on",
			`{"protocol":"vless","mux":{"enabled":true,"concurrency":8},"settings":{"vnext":[{"address":"h.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"xhttp","xhttpSettings":{"path":"/","host":"h.example","mode":"auto"}}}`},
		{"vless grpc", "vless://" + uid + "@g.example:443?type=grpc&security=tls&serviceName=svc&mode=multi&authority=auth.example",
			`{"protocol":"vless","settings":{"vnext":[{"address":"g.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"grpc","grpcSettings":{"serviceName":"svc","multiMode":true,"authority":"auth.example"},"security":"tls","tlsSettings":{"serverName":"g.example"}}}`},
		// serviceName ?? path: path used only when serviceName is absent
		{"vless grpc path", "vless://" + uid + "@g.example:443?type=GRPC&path=p1",
			`{"protocol":"vless","settings":{"vnext":[{"address":"g.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"grpc","grpcSettings":{"serviceName":"p1","multiMode":false}}}`},
		{"vless tcp http header", "vless://" + uid + "@t.example:80?type=tcp&headerType=http&host=a.com,b.com&path=/x",
			`{"protocol":"vless","settings":{"vnext":[{"address":"t.example","port":80,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"tcpSettings":{"header":{"type":"http","request":{"path":["/x"],"headers":{"Host":["a.com","b.com"]}}}},"network":"tcp"}}`},
		{"vless raw http header no host", "vless://" + uid + "@t.example:80?type=raw&headerType=http",
			`{"protocol":"vless","settings":{"vnext":[{"address":"t.example","port":80,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"tcpSettings":{"header":{"type":"http","request":{"path":["/"]}}},"network":"tcp"}}`},
		{"vless httpupgrade", "vless://" + uid + "@u.example:8080?type=httpupgrade&path=/u&security=none",
			`{"protocol":"vless","settings":{"vnext":[{"address":"u.example","port":8080,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"httpupgrade","httpupgradeSettings":{"path":"/u","host":"u.example"}}}`},
		{"vless kcp plain", "vless://" + uid + "@k.example:1000?type=kcp&headerType=none",
			`{"protocol":"vless","settings":{"vnext":[{"address":"k.example","port":1000,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"mkcp"}}`},
		// last value of a repeated key wins (Uri.splitQueryString); '+' is a space
		{"vless repeated key", "vless://" + uid + "@d.example:1?type=ws&path=/a&path=/b+c",
			`{"protocol":"vless","settings":{"vnext":[{"address":"d.example","port":1,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"ws","wsSettings":{"path":"/b c","headers":{}}}}`},
		{"vless ipv6 fm", "vless://" + uid + "@[2001:DB8::1]:443?security=tls&sni=v6.example&fm=" + url.QueryEscape(fm),
			`{"protocol":"vless","settings":{"vnext":[{"address":"2001:db8::1","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
			  "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"v6.example"},"finalmask":{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"100-200"}}]}}}`},
		{"vmess ws tls", "vmess://" + b64std(vmessWS),
			`{"protocol":"vmess","settings":{"vnext":[{"address":"vm.example","port":8080,"users":[{"id":"` + uid + `","alterId":0,"security":"auto"}]}]},
			  "streamSettings":{"network":"ws","wsSettings":{"path":"/vm","headers":{"Host":"wsh.example"}},"security":"tls","tlsSettings":{"serverName":"wsh.example","fingerprint":"chrome"}}}`},
		// port 443.0 -> "443.0" -> tryParse fails -> 443; aid "2"; tls "none" -> plaintext; vmess grpc uses path as serviceName
		{"vmess grpc urlsafe", "vmess://" + b64url(vmessGRPC),
			`{"protocol":"vmess","settings":{"vnext":[{"address":"g.example","port":443,"users":[{"id":"` + uid + `","alterId":2,"security":"aes-128-gcm"}]}]},
			  "streamSettings":{"network":"grpc","grpcSettings":{"serviceName":"svc","multiMode":false}}}`},
		// trojan: no security= means TLS; %40 in userinfo decoded
		{"trojan default tls", "trojan://p%40ss@tr.example:443?sni=tsni.example#x",
			`{"protocol":"trojan","settings":{"servers":[{"address":"tr.example","port":443,"password":"p@ss"}]},
			  "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"tsni.example"}}}`},
		{"trojan none ws", "trojan://pw@1.1.1.1:80?security=none&type=ws&host=h.example",
			`{"protocol":"trojan","settings":{"servers":[{"address":"1.1.1.1","port":80,"password":"pw"}]},
			  "streamSettings":{"network":"ws","wsSettings":{"path":"/","headers":{"Host":"h.example"}},"security":"none"}}`},
		{"trojan reality", "trojan://pw@tr.example:443?security=reality&pbk=K&sid=01&sni=www.apple.com",
			`{"protocol":"trojan","settings":{"servers":[{"address":"tr.example","port":443,"password":"pw"}]},
			  "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"www.apple.com","fingerprint":"chrome","publicKey":"K","shortId":"01","spiderX":"/"}}}`},
		// trojan security="" is not 'none' -> TLS
		{"trojan empty security", "trojan://pw@tr.example?security=",
			`{"protocol":"trojan","settings":{"servers":[{"address":"tr.example","port":443,"password":"pw"}]},
			  "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"tr.example"}}}`},
		{"ss sip002 b64", "ss://" + b64url("chacha20-ietf-poly1305:secret") + "@ss.example:8388#tag",
			`{"protocol":"shadowsocks","settings":{"servers":[{"address":"ss.example","port":8388,"method":"chacha20-ietf-poly1305","password":"secret"}]},
			  "streamSettings":{"network":"tcp"}}`},
		{"ss plain v6", "ss://2022-blake3-aes-128-gcm:a2V5%3D@[2001:db8::1]:8388",
			`{"protocol":"shadowsocks","settings":{"servers":[{"address":"2001:db8::1","port":8388,"method":"2022-blake3-aes-128-gcm","password":"a2V5="}]},
			  "streamSettings":{"network":"tcp"}}`},
		{"ss legacy", "ss://" + b64std("aes-256-gcm:pw@1.2.3.4:443") + "#%D0%B8%D0%BC%D1%8F",
			`{"protocol":"shadowsocks","settings":{"servers":[{"address":"1.2.3.4","port":443,"method":"aes-256-gcm","password":"pw"}]},
			  "streamSettings":{"network":"tcp"}}`},
		{"ss ws tls", "ss://" + b64url("aes-128-gcm:pw") + "@s.example:443?type=ws&security=tls&host=w.example&path=/s",
			`{"protocol":"shadowsocks","settings":{"servers":[{"address":"s.example","port":443,"method":"aes-128-gcm","password":"pw"}]},
			  "streamSettings":{"network":"ws","wsSettings":{"path":"/s","headers":{"Host":"w.example"}},"security":"tls","tlsSettings":{"serverName":"w.example"}}}`},
		// hysteria2: whole userinfo is the auth; salamander -> finalmask (bot parity, see report)
		{"hy2 salamander", "hy2://auth:pass@hy.example:4443?sni=hsni.example&obfs=salamander&obfs-password=op#n",
			`{"protocol":"hysteria","settings":{"address":"hy.example","port":4443,"version":2},
			  "streamSettings":{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"hsni.example","alpn":["h3"]},"hysteriaSettings":{"auth":"auth:pass","version":2},
			  "finalmask":{"udp":[{"type":"salamander","settings":{"password":"op"}}]}}}`},
		{"hysteria2 plain", "HYSTERIA2://pw@h2.example",
			`{"protocol":"hysteria","settings":{"address":"h2.example","port":443,"version":2},
			  "streamSettings":{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"h2.example","alpn":["h3"]},"hysteriaSettings":{"auth":"pw","version":2}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := parseVpnURI(c.uri)
			if k == nil || !k.supported() {
				t.Fatalf("not supported: %+v", k)
			}
			ob, err := buildOutbound(k)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canon(t, ob), canonStr(t, c.want); got != want {
				t.Errorf("\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestUntestable(t *testing.T) {
	cases := []struct{ uri, reason string }{
		{"tuic://x@h:1", "scheme_unsupported_by_core:tuic"},
		{"hysteria://x@h:1", "scheme_unsupported_by_core:hysteria"},
		{"wireguard://k@h:51820", "scheme_unsupported_by_core:wireguard"},
		{"vless://" + uid + "@h.example:443?type=h2", "transport_removed_from_core:h2"},
		{"vless://" + uid + "@h.example:443?type=QUIC", "transport_removed_from_core:quic"},
		{"vmess://" + b64std(`{"add":"h","port":"1","id":"x","net":"http"}`), "transport_removed_from_core:http"},
		{"vless://" + uid + "@h.example:443?type=kcp&seed=abc", "mkcp_header_seed_removed_from_core:kcp"},
		{"vless://" + uid + "@h.example:443?type=mkcp&headerType=wechat-video", "mkcp_header_seed_removed_from_core:mkcp"},
		{"vless://" + uid + "@h.example:abc", "parse_error:"},
		{"vless://" + uid + "@[zz::1]:443", "parse_error:"},
		{"vmess://not-base64!!", "parse_error:"},
		{"vmess://" + b64std(`[1,2]`), "parse_error:"},
		{"ss://" + b64std("aes-256-gcm:pw") + "@h.example:8388#Россия", "parse_error:"}, // raw non-ASCII tag: Uri.decodeComponent throws
		{"ss://" + b64std("aes-256-gcm:pw-no-at"), "parse_error:"},
		{"ss://" + b64std("aes-256-gcm:pw") + "@h.example", "parse_error:"},
		{"ss://" + b64std("aes-256-gcm:pw") + "@h.example:8388/?plugin=x", "missing_fields"}, // "8388/" is no port
	}
	for _, c := range cases {
		o := checkVpnKey(context.Background(), Env{}, c.uri, nil, "", "", nil)
		got, _ := o.untestableReason.(string)
		if !o.untestable || !strings.HasPrefix(got, c.reason) || (c.reason == "parse_error:" && strings.Contains(got, "h.example")) {
			t.Errorf("%s: untestable=%v reason=%q, want %q", c.uri, o.untestable, got, c.reason)
		}
	}
	for _, s := range []string{"hello", " vless://" + uid + "@h:1", "http://x.example/sub", "ftp://h"} {
		o := checkVpnKey(context.Background(), Env{}, s, nil, "", "", nil)
		if o.untestableReason != "unrecognized_scheme" {
			t.Errorf("%q: %v", s, o.untestableReason)
		}
	}
	// The JSON shape of an untestable result (VpnCheckResult.toJson).
	o := checkVpnKey(context.Background(), Env{}, "tuic://x@h:1", nil, "", "next", nil)
	want := `{"connected":false,"error":null,"latency_ms":null,"sites":[],"sockets_bound":0,"sockets_unbound":0,"untestable":true,"untestable_reason":"scheme_unsupported_by_core:tuic","works":false,"xray_core":"26.10"}`
	if got := canon(t, o.toJSON()); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestFullConfigAndDesktop(t *testing.T) {
	k := parseVpnURI("vless://" + uid + "@srv.example:443?security=tls&sni=s.example")
	cfg, err := buildXrayConfig(k, 23456)
	if err != nil {
		t.Fatal(err)
	}
	android := `{"log":{"loglevel":"warning"},"dns":{"servers":["8.8.8.8","1.1.1.1","localhost"],"queryStrategy":"UseIPv4"},
	  "inbounds":[{"listen":"127.0.0.1","port":23456,"protocol":"http","settings":{}}],
	  "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"srv.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
	    "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"s.example"}}}]}`
	if got, want := canon(t, cfg), canonStr(t, android); got != want {
		t.Fatalf("android config\n got %s\nwant %s", got, want)
	}
	desktopXrayConfig(cfg, map[string]any{"interface": "wwan0"}, []netip.Addr{netip.MustParseAddr("10.64.64.64"), netip.MustParseAddr("127.0.0.53"), netip.MustParseAddr("8.8.8.8")})
	desk := `{"log":{"loglevel":"warning"},"dns":{"servers":["10.64.64.64","8.8.8.8","1.1.1.1"],"queryStrategy":"UseIPv4","tag":"svz-dns"},
	  "inbounds":[{"listen":"127.0.0.1","port":23456,"protocol":"http","settings":{}}],
	  "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"srv.example","port":443,"users":[{"id":"` + uid + `","encryption":"none"}]}]},
	    "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"s.example"},"sockopt":{"interface":"wwan0","domainStrategy":"UseIPv4"}}},
	    {"tag":"svz-direct","protocol":"freedom","settings":{"domainStrategy":"AsIs"},"streamSettings":{"sockopt":{"interface":"wwan0"}}}],
	  "routing":{"domainStrategy":"AsIs","rules":[{"type":"field","inboundTag":["svz-dns"],"outboundTag":"svz-direct"}]}}`
	if got, want := canon(t, cfg), canonStr(t, desk); got != want {
		t.Fatalf("desktop config\n got %s\nwant %s", got, want)
	}
}

// Every generated config must load in both cores (xray run -test, config
// over stdin). SVZ_XRAY / SVZ_XRAY_NEXT = paths of the two binaries.
func TestConfigsLoadInCores(t *testing.T) {
	bins := map[string]string{"26.3": os.Getenv("SVZ_XRAY"), "26.10": os.Getenv("SVZ_XRAY_NEXT")}
	if bins["26.3"] == "" {
		t.Skip("SVZ_XRAY not set")
	}
	keys := []string{
		"vless://" + uid + "@1.2.3.4:443?type=tcp&security=reality&sni=www.microsoft.com&fp=firefox&pbk=" + testPBK + "&sid=ab12&flow=xtls-rprx-vision",
		"vless://" + uid + "@1.2.3.4?type=ws&security=tls&host=cdn.example.org&path=%2Fws&alpn=h2,http/1.1",
		"vless://" + uid + "@1.2.3.4:443?type=xhttp&security=tls&sni=s.example&mode=packet-up&extra=" + url.QueryEscape(`{"xmux":{"cMaxLifetimeMs":0.0,"maxConcurrency":"16-32"},"xPaddingBytes":"100-1000"}`),
		"vless://" + uid + "@1.2.3.4:443?type=grpc&security=tls&serviceName=svc&mode=multi",
		"vless://" + uid + "@1.2.3.4:80?type=tcp&headerType=http&host=a.com&path=/x&security=tls",
		"vless://" + uid + "@1.2.3.4:8080?type=httpupgrade&path=/u&security=tls",
		"vless://" + uid + "@1.2.3.4:1000?type=kcp&security=tls",
		"vless://" + uid + "@1.2.3.4:443?security=tls&mux=1",
		"vmess://" + b64std(`{"add":"1.2.3.4","port":"8080","id":"`+uid+`","aid":0,"net":"ws","host":"h","path":"/vm","tls":"tls","fp":"chrome"}`),
		"trojan://pw@1.2.3.4:443?sni=t.example",
		"trojan://pw@1.2.3.4:443?security=reality&pbk=" + testPBK + "&sid=01&sni=www.apple.com",
		"ss://" + b64url("chacha20-ietf-poly1305:secret") + "@1.2.3.4:8388",
		"ss://2022-blake3-aes-128-gcm:" + url.QueryEscape("MTIzNDU2Nzg5MDEyMzQ1Ng==") + "@1.2.3.4:8388",
		"hy2://auth@1.2.3.4:4443?sni=h.example&obfs=salamander&obfs-password=op",
		"vless://" + uid + "@1.2.3.4:443?security=tls&fm=" + url.QueryEscape(`{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"100-200","delay":"10-20"}}]}`),
	}
	for core, bin := range bins {
		if bin == "" {
			continue
		}
		for _, uri := range keys {
			k := parseVpnURI(uri)
			cfg, err := buildXrayConfig(k, 23456)
			if err != nil {
				t.Fatal(err)
			}
			desktopXrayConfig(cfg, map[string]any{"interface": "lo"}, []netip.Addr{netip.MustParseAddr("10.64.64.64")})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			cmd := exec.CommandContext(ctx, bin, "run", "-test", "-config", "stdin:")
			cmd.Stdin = bytes.NewReader(marshalConfig(cfg))
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				t.Errorf("core %s rejects %s:\n%s", core, k.Protocol+"/"+k.network(), lastLines(string(out), 3))
			}
		}
	}
}

// a valid x25519 public key (any 32 bytes, base64url)
const testPBK = "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0"

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func TestDartHelpers(t *testing.T) {
	for in, want := range map[string]int{" 42 ": 42, "+7": 7, "-3": -3, "0x1F": 31} {
		if got, ok := dartTryParseInt(in); !ok || got != want {
			t.Errorf("tryParse(%q)=%d,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "4.0", "1e3", "12a", "0x"} {
		if _, ok := dartTryParseInt(in); ok {
			t.Errorf("tryParse(%q) should fail", in)
		}
	}
	if s := dartNumberString("443.0"); s != "443.0" {
		t.Error(s)
	}
	if s := dartNumberString("4e2"); s != "400.0" {
		t.Error(s)
	}
	if v, err := lenientDecode("a%zzb%41+c", true); err != nil || v != "a%zzbA c" {
		t.Errorf("lenient %q %v", v, err)
	}
	if _, err := strictDecode("a%zz", false); err == nil {
		t.Error("strict should fail")
	}
	if _, err := strictDecode("имя", false); err == nil {
		t.Error("strict should fail on non-ASCII")
	}
}

func TestMasker(t *testing.T) {
	var lines []string
	var secrets []string
	m := &masker{env: Env{Log: func(s string) { lines = append(lines, s) }, Secret: func(s string) { secrets = append(secrets, s) }}}
	key := "vless://" + uid + "@srv.example:443?security=reality&pbk=PUBKEYVALUE&sid=ab"
	m.hideResource(key)
	m.log("dial tcp srv.example:443 with PUBKEYVALUE / " + key)
	if strings.Contains(lines[0], "srv.example") || strings.Contains(lines[0], "PUBKEYVALUE") || strings.Contains(lines[0], uid) {
		t.Errorf("leak: %s", lines[0])
	}
	if len(secrets) < 3 {
		t.Errorf("secrets %v", secrets)
	}
}
