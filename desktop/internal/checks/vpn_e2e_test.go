package checks

// VPN keys end to end: a local xray server (vless tcp, vless ws, vless
// reality+vision, trojan tls, shadowsocks) on 127.0.0.1, the check through
// the built cores. SVZ_XRAY (+ SVZ_XRAY_NEXT) = core binaries; the reality
// key needs internet (its dest), probe sites go to the internet through
// the tunnel.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	p, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func selfSigned(t *testing.T, dir string) (crt, key string) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	crt, key = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return
}

func x25519(t *testing.T, bin string) (priv, pub string) {
	out, err := exec.Command(bin, "x25519").CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	re := regexp.MustCompile(`(?m)^(Private ?[Kk]ey|PrivateKey|Public ?[Kk]ey|Password)[^:]*:\s*(\S+)`)
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		if strings.HasPrefix(strings.ToLower(m[1]), "private") {
			priv = m[2]
		} else if pub == "" {
			pub = m[2]
		}
	}
	t.Logf("x25519: %s", strings.ReplaceAll(string(out), "\n", " | "))
	if priv == "" || pub == "" {
		t.Fatalf("x25519 output: %s", out)
	}
	return
}

func children(t *testing.T) int {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	return len(strings.Fields(string(out)))
}

func TestVPNEndToEnd(t *testing.T) {
	bin := os.Getenv("SVZ_XRAY")
	next := os.Getenv("SVZ_XRAY_NEXT")
	if bin == "" {
		t.Skip("SVZ_XRAY not set")
	}
	dir := t.TempDir()
	crt, key := selfSigned(t, dir)
	priv, pub := x25519(t, bin)
	pVless, pTrojan, pSS, pReality, pWS, pDead := freePort(t), freePort(t), freePort(t), freePort(t), freePort(t), freePort(t)
	server := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": pVless, "protocol": "vless",
				"settings": map[string]any{"clients": []any{map[string]any{"id": uid}}, "decryption": "none"}},
			map[string]any{"listen": "127.0.0.1", "port": pTrojan, "protocol": "trojan",
				"settings": map[string]any{"clients": []any{map[string]any{"password": "trpw"}}},
				"streamSettings": map[string]any{"network": "tcp", "security": "tls",
					"tlsSettings": map[string]any{"certificates": []any{map[string]any{"certificateFile": crt, "keyFile": key}}}}},
			map[string]any{"listen": "127.0.0.1", "port": pSS, "protocol": "shadowsocks",
				"settings": map[string]any{"method": "aes-256-gcm", "password": "sspw", "network": "tcp,udp"}},
			map[string]any{"listen": "127.0.0.1", "port": pReality, "protocol": "vless",
				"settings": map[string]any{"clients": []any{map[string]any{"id": uid, "flow": "xtls-rprx-vision"}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "reality",
					"realitySettings": map[string]any{"dest": "www.google.com:443", "serverNames": []any{"www.google.com"},
						"privateKey": priv, "shortIds": []any{"", "ab12"}}}},
			map[string]any{"listen": "127.0.0.1", "port": pWS, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": uid}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "ws", "wsSettings": map[string]any{"path": "/ws"}}},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom"}},
	}
	sb, _ := json.Marshal(server)
	srv := exec.Command(bin, "run", "-config", "stdin:")
	srv.Stdin = bytes.NewReader(sb)
	var srvOut bytes.Buffer
	srv.Stdout, srv.Stderr = &srvOut, &srvOut
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Process.Kill(); srv.Wait() }()
	time.Sleep(800 * time.Millisecond)

	// ~1 MB file and a file the "DPI" cuts after 20 KB.
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cut" {
			w.Header().Set("Content-Length", "1000000")
			w.Write(bytes.Repeat([]byte("x"), 20000))
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
			return
		}
		w.Header().Set("Content-Length", "1000000")
		w.Write(bytes.Repeat([]byte("y"), 1000000))
	}))
	defer files.Close()

	// The trojan server's self-signed certificate: trusted by the client
	// core through SSL_CERT_FILE (inherited by the child).
	os.Setenv("SSL_CERT_FILE", crt)
	defer os.Unsetenv("SSL_CERT_FILE")

	keys := []struct {
		name, uri string
		works     bool
		cut       bool
	}{
		{"vless tcp", fmt.Sprintf("vless://%s@127.0.0.1:%d?type=tcp&security=none#t", uid, pVless), true, false},
		{"vless ws", fmt.Sprintf("vless://%s@127.0.0.1:%d?type=ws&path=%%2Fws&host=localhost", uid, pWS), true, false},
		{"vless reality vision", fmt.Sprintf("vless://%s@127.0.0.1:%d?security=reality&sni=www.google.com&fp=chrome&pbk=%s&sid=ab12&flow=xtls-rprx-vision", uid, pReality, pub), true, false},
		{"trojan tls", fmt.Sprintf("trojan://trpw@127.0.0.1:%d?sni=localhost", pTrojan), true, false},
		{"shadowsocks", "ss://" + b64url("aes-256-gcm:sspw") + "@127.0.0.1:" + strconv.Itoa(pSS) + "#ss", true, false},
		{"shadowsocks cut file", "ss://" + b64url("aes-256-gcm:sspw") + "@127.0.0.1:" + strconv.Itoa(pSS), false, true},
		{"dead key", fmt.Sprintf("vless://%s@127.0.0.1:%d?type=tcp", uid, pDead), false, false},
		{"wrong password", fmt.Sprintf("trojan://nope@127.0.0.1:%d?sni=localhost", pTrojan), false, false},
	}
	cores := map[string]string{"": bin}
	if next != "" {
		cores["next"] = next
	}
	b := newTestBinder(map[string]any{"interface": "lo"}) // the server is on loopback
	before := children(t)
	for coreName, _ := range cores {
		for _, k := range keys {
			t.Run(fmt.Sprintf("%s/%s", map[string]string{"": "26.3", "next": "26.10"}[coreName], k.name), func(t *testing.T) {
				if coreName == "next" && strings.HasPrefix(k.name, "trojan") {
					// 26.10 checks certificates against its bundled Mozilla
					// roots, not the system pool: SSL_CERT_FILE cannot make
					// the test's self-signed server trusted there.
					t.Skip("26.10 ignores SSL_CERT_FILE")
				}
				var logs []string
				env := Env{Net: b, XrayPath: bin, XrayNextPath: next, APIBase: "http://127.0.0.1:1",
					Log: func(s string) { logs = append(logs, s) }, Secret: func(string) {}}
				dl := files.URL + "/1mb"
				if k.cut {
					dl = files.URL + "/cut"
				}
				resetCellIP()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				ok, r := Run(ctx, env, Job{ID: 100, Resource: k.uri, ResourceType: "vpn_key", NeedsCore: coreName,
					ProbeSites: []string{"google.com", "telegram.org"}, DownloadURL: dl})
				r2 := copyMap(r)
				delete(r2, "cellular_ip")
				dump(t, "result", r2)
				if !ok {
					t.Errorf("success=false (only untestable keys fail)")
				}
				if r["works"] != k.works {
					t.Errorf("works=%v want %v; error=%v\nclient log:\n%s\nserver log:\n%s", r["works"], k.works, r["error"],
						strings.Join(logs, "\n"), lastLines(srvOut.String(), 8))
				}
				wantCore := "26.3"
				if coreName == "next" {
					wantCore = "26.10"
				}
				if r["xray_core"] != wantCore {
					t.Errorf("xray_core %v", r["xray_core"])
				}
				if r["sockets_bound"].(int) == 0 || r["sockets_unbound"].(int) != 0 {
					t.Errorf("sockets %v/%v", r["sockets_bound"], r["sockets_unbound"])
				}
				if k.cut {
					d := r["download"].(map[string]any)
					if d["ok"] != false || d["bytes"].(int) < 10000 || d["bytes"].(int) > 100000 || d["expected_bytes"] != 1000000 {
						t.Errorf("cut download %v", d)
					}
				}
				for _, l := range logs {
					if strings.Contains(l, k.uri) || strings.Contains(l, "trpw") || strings.Contains(l, uid) {
						t.Errorf("secret in log: %s", l)
					}
				}
				if c := children(t); c != before {
					t.Errorf("xray left running: %d children, want %d", c, before)
				}
			})
		}
	}

	// Core failures.
	env := Env{Net: b, XrayPath: bin, XrayNextPath: "/nonexistent/xray-next", APIBase: "http://127.0.0.1:1"}
	resetCellIP()
	_, r := Run(context.Background(), env, Job{ID: 101, Resource: keys[0].uri, ResourceType: "vpn_key", NeedsCore: "next"})
	if r["error"] != "xray: xray 26.10 не найден в программе" || r["sockets_unbound"] != 1 || r["connected"] != false {
		t.Errorf("missing next core: %v", r)
	}
	bad := fmt.Sprintf("vless://%s@127.0.0.1:%d?type=xhttp&extra=%s", uid, pVless, url.QueryEscape(`{"xmux":{"maxConcurrency":"abc"}}`))
	_, r = Run(context.Background(), Env{Net: b, XrayPath: bin, APIBase: "http://127.0.0.1:1"}, Job{ID: 102, Resource: bad, ResourceType: "vpn_key"})
	dump(t, "config rejected", r)
	if e, _ := r["error"].(string); !strings.HasPrefix(e, "xray: ") || r["connected"] != false {
		t.Errorf("bad config: %v", r)
	}
	// The server given by name: resolved by xray's own DNS (domainStrategy
	// UseIPv4, queries through the svz-direct outbound), not the system
	// resolver. localtest.me is a public name for 127.0.0.1; the binder's
	// sockopt carries no interface here (a lo-bound DNS query cannot reach
	// a public resolver).
	resetCellIP()
	_, r = Run(context.Background(), Env{Net: newTestBinder(map[string]any{"tcpFastOpen": false}), XrayPath: bin, APIBase: "http://127.0.0.1:1"},
		Job{ID: 104, Resource: fmt.Sprintf("vless://%s@localtest.me:%d?type=tcp", uid, pVless), ResourceType: "vpn_key", ProbeSites: []string{"google.com"}})
	if r["works"] != true || r["sockets_bound"].(int) == 0 {
		t.Errorf("server by name: %v", r)
	}
	// No sockopt from the binder: everything counts as unbound.
	resetCellIP()
	_, r = Run(context.Background(), Env{Net: newTestBinder(nil), XrayPath: bin, APIBase: "http://127.0.0.1:1"},
		Job{ID: 103, Resource: keys[0].uri, ResourceType: "vpn_key", ProbeSites: []string{"google.com"}})
	if r["sockets_bound"] != 0 || r["sockets_unbound"].(int) == 0 || r["bind_error"] == nil {
		t.Errorf("unbound: %v", r)
	}
	_ = net.IPv4len
}
