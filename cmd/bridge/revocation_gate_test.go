package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentry/agentry/pkg/bridge"
)

func reqWithCN(cn string) *http.Request {
	r := httptest.NewRequest("GET", "https://bridge.agentry.run/api/clusters", nil)
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: cn}}},
	}
	return r
}

// mtlsGate must reject a handshake whose peer-cert CN is on the pushed
// revocation denylist — that's what makes "delete a cluster" actually
// stop trusting its still-unexpired cert.
func TestMtlsGate_RejectsRevokedCert(t *testing.T) {
	// DevMode broker so the admin-gated revoked-cns PUT is accepted in
	// the test without minting a real admin cert.
	b := bridge.NewWithConfig(bridge.Config{DevMode: true})
	pr := httptest.NewRequest("PUT", "/api/revoked-cns", bytes.NewBufferString(`{"cns":["cluster-evil"]}`))
	pw := httptest.NewRecorder()
	b.Handler().ServeHTTP(pw, pr)
	if pw.Code != http.StatusNoContent {
		t.Fatalf("seeding revoked-cns failed: code=%d", pw.Code)
	}

	var reached bool
	gate := mtlsGate(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// Revoked cert → 403, inner handler never runs.
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, reqWithCN("cluster-evil"))
	if w.Code != http.StatusForbidden || reached {
		t.Errorf("revoked cert: code=%d reached=%v; want 403 + not reached", w.Code, reached)
	}

	// Non-revoked cert → passes through.
	reached = false
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, reqWithCN("cluster-good"))
	if w.Code != http.StatusOK || !reached {
		t.Errorf("good cert: code=%d reached=%v; want 200 + reached", w.Code, reached)
	}

	// No cert at all → 401 (unchanged behavior).
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest("GET", "https://bridge/api/clusters", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no cert: code=%d; want 401", w.Code)
	}
}
