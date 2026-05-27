package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func decodeJSON(t *testing.T, body io.Reader, into any) {
	t.Helper()
	if err := json.NewDecoder(body).Decode(into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestCreateWithTTLReturnsExpiresAt(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(`{"sandbox_id":"s1","ttl_seconds":3600}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var got SandboxInfo
	decodeJSON(t, resp.Body, &got)
	if got.ExpiresAt == "" {
		t.Fatal("expires_at empty; want RFC3339 timestamp")
	}
	when, err := time.Parse(time.RFC3339, got.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at not RFC3339: %v", err)
	}
	if d := time.Until(when); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("expires_at ~+1h expected, got %s away", d)
	}
}

func TestCreateWithoutTTLOmitsExpiresAt(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(`{"sandbox_id":"s1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got SandboxInfo
	decodeJSON(t, resp.Body, &got)
	if got.ExpiresAt != "" {
		t.Fatalf("expires_at = %q; want empty for no-TTL sandbox", got.ExpiresAt)
	}
}

func TestRenewExtendsExpiry(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")

	// Create with TTL=1h.
	mustCreate(t, ts, `{"sandbox_id":"s1","ttl_seconds":3600}`)
	annsBefore, _ := mock.GetPodAnnotations(context.Background(), "default", "sandbox-s1")
	beforeExpiry, _ := parseExpiresAt(annsBefore[AnnotationExpiresAt])

	// Sleep a moment so the new timestamp is observably different.
	time.Sleep(50 * time.Millisecond)

	// Renew with explicit TTL=7200.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes/s1/renew",
		bytes.NewBufferString(`{"ttl_seconds":7200}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("renew status = %d body=%s", resp.StatusCode, body)
	}
	var renewed map[string]any
	decodeJSON(t, resp.Body, &renewed)
	gotExpiry, _ := parseExpiresAt(renewed["expires_at"].(string))
	if !gotExpiry.After(beforeExpiry) {
		t.Fatalf("renewed expiry %s not after original %s", gotExpiry, beforeExpiry)
	}

	// TTL annotation should also reflect the new value.
	ann, _ := mock.GetPodAnnotations(context.Background(), "default", "sandbox-s1")
	if ann[AnnotationTTLSec] != "7200" {
		t.Errorf("ttl-seconds annotation = %q; want 7200", ann[AnnotationTTLSec])
	}
}

func TestRenewWithoutBodyReusesStoredTTL(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")
	mustCreate(t, ts, `{"sandbox_id":"s1","ttl_seconds":600}`)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes/s1/renew", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	ann, _ := mock.GetPodAnnotations(context.Background(), "default", "sandbox-s1")
	if ann[AnnotationTTLSec] != "600" {
		t.Errorf("ttl-seconds annotation = %q; want 600 (reused)", ann[AnnotationTTLSec])
	}
}

func TestRenewWithoutPriorTTLRejected(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")
	mustCreate(t, ts, `{"sandbox_id":"s1"}`) // no TTL

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes/s1/renew", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 when no prior TTL", resp.StatusCode)
	}
}

func TestRenewMissingSandboxReturns404(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes/nope/renew",
		bytes.NewBufferString(`{"ttl_seconds":60}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", resp.StatusCode)
	}
}

func TestRenewNegativeTTLRejected(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")
	mustCreate(t, ts, `{"sandbox_id":"s1","ttl_seconds":60}`)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes/s1/renew",
		bytes.NewBufferString(`{"ttl_seconds":-5}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
}

func TestGetSurfacesExpiresAt(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")
	mustCreate(t, ts, `{"sandbox_id":"s1","ttl_seconds":3600}`)

	resp, err := http.Get(ts.URL + "/api/sandboxes/s1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info SandboxInfo
	decodeJSON(t, resp.Body, &info)
	if info.ExpiresAt == "" {
		t.Fatal("GET did not include expires_at")
	}
	if _, err := time.Parse(time.RFC3339, info.ExpiresAt); err != nil {
		t.Errorf("expires_at not parseable: %v", err)
	}
}

func TestListSurfacesExpiresAt(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")
	mustCreate(t, ts, `{"sandbox_id":"with","ttl_seconds":3600}`)
	mustCreate(t, ts, `{"sandbox_id":"without"}`)

	resp, err := http.Get(ts.URL + "/api/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Sandboxes []SandboxInfo `json:"sandboxes"`
	}
	decodeJSON(t, resp.Body, &got)

	byID := make(map[string]SandboxInfo, len(got.Sandboxes))
	for _, s := range got.Sandboxes {
		byID[s.SandboxID] = s
	}
	if v := byID["with"].ExpiresAt; v == "" {
		t.Errorf("TTL sandbox missing expires_at in list")
	}
	if v := byID["without"].ExpiresAt; v != "" {
		t.Errorf("no-TTL sandbox expires_at = %q; want empty", v)
	}
}

func mustCreate(t *testing.T, ts *httptest.Server, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %q: status=%d body=%s", strings.TrimSpace(body), resp.StatusCode, b)
	}
}
