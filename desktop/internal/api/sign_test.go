package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"testing"
)

// TestSignDump: SIG_DUMP=file writes {pem, data, sig} for an independent
// check with Node's crypto.verify('sha256', data, pem, sig) -- what the
// server's requireSignature runs.
func TestSignDump(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := PublicPEM(k)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, d := range []string{"GET /executor/v1/jobs/poll 1763726400000", "POST /executor/v1/jobs/42/result 1763726400123", "AbCd+/==challenge"} {
		sig, err := SignString(k, d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]string{"pem": pem, "data": d, "sig": sig})
	}
	if p := os.Getenv("SIG_DUMP"); p != "" {
		b, _ := json.Marshal(out)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
