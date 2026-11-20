package auth

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tlsKeySet(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

// An issuer serving a certificate from a private CA is refused until the CA file is trusted, then fetched.
func TestJWKSTrustCAFile(t *testing.T) {
	srv, ca := tlsKeySet(t)

	plain, err := NewJWKS(srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plain.fetch(context.Background()); err == nil {
		t.Fatal("a certificate from an unknown CA was accepted")
	}

	j, err := NewJWKS(srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.TrustCAFile(ca); err != nil {
		t.Fatal(err)
	}
	// The answer holds no key, so the fetch may still fail; it must not fail in the TLS handshake.
	if _, _, err := j.fetch(context.Background()); err != nil && strings.Contains(err.Error(), "x509") {
		t.Fatalf("the trusted CA was not used: %v", err)
	}
}

func TestJWKSTrustCAFileRefusals(t *testing.T) {
	j, _ := NewJWKS("https://dex.example/keys", false)
	if err := j.TrustCAFile(filepath.Join(t.TempDir(), "missing.crt")); err == nil {
		t.Fatal("a missing file was accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk.crt")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := j.TrustCAFile(junk); err == nil {
		t.Fatal("a file with no certificate was accepted")
	}
}
