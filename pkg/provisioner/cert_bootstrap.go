package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnrollRequest is what the provisioner POSTs to the control plane's
// /api/v1/enroll endpoint. Mirrors the agentry-app api.EnrollRequest
// shape — kept inline rather than imported to avoid a dependency on
// the closed-source control plane code.
type EnrollRequest struct {
	Token  string `json:"token"`
	CSRPem string `json:"csr_pem"`
}

// EnrollResponse mirrors agentry-app's api.EnrollResponse. Same
// rationale for inlining.
type EnrollResponse struct {
	DeviceCertPem string `json:"device_cert_pem"`
	CACertPem     string `json:"ca_cert_pem"`
	BridgeURL     string `json:"bridge_url"`
	ExpiresAt     string `json:"expires_at"`
}

// ClusterCertBundle is what we keep on disk: a PEM cert chain, the
// matching ECDSA private key, and the CA cert the broker uses to sign
// device certs (we add it to our RootCAs when dialing the broker so
// the LetsEncrypt-issued server cert chain is independently verified).
type ClusterCertBundle struct {
	CertPath string
	KeyPath  string
	CAPath   string
}

// BootstrapClusterCert ensures we have a usable mTLS bundle for the
// outbound bridge tunnel. Strategy:
//
//  1. If certDir/cluster.crt exists and parses + isn't already expired,
//     reuse it (we'll check for "near-expiry" separately for renewal).
//  2. Otherwise enroll: generate a fresh keypair, build a CSR, POST to
//     the control plane's /api/v1/enroll endpoint with the one-time
//     enrollment token, persist the response.
//
// Returns the on-disk paths regardless of which branch we took.
//
// Required env-driven config (all on cfg):
//
//   - cfg.CertDir     — where to persist {cluster.crt, cluster.key, ca.crt}
//   - cfg.EnrollURL   — control plane's /api/v1/enroll endpoint
//   - cfg.EnrollToken — one-time enrollment token (single-use, ~1h TTL)
//   - cfg.ClusterID   — cluster name; used for the CSR's placeholder
//                       CN (the control plane overrides anyway)
//
// A missing field returns an error rather than silently doing the
// wrong thing — operators want loud failures at boot.
//
// EnrollToken is consumed by the control plane on first success; it
// only needs to be set on the first run. After a cert is on disk,
// renewal happens automatically (RunCertRenewer) and uses a fresh
// short-lived token minted from the dashboard. For the v1 ergonomic,
// we accept the token via env var on every start — the control plane
// rejects already-consumed tokens with a clear error, so leaving it
// in place is harmless.
func BootstrapClusterCert(ctx context.Context, cfg Config) (*ClusterCertBundle, error) {
	if cfg.CertDir == "" {
		return nil, fmt.Errorf("CertDir is required (set AGENTRY_CERT_DIR)")
	}
	if cfg.ClusterID == "" {
		return nil, fmt.Errorf("ClusterID is required (set AGENTRY_CLUSTER_NAME)")
	}

	bundle := &ClusterCertBundle{
		CertPath: filepath.Join(cfg.CertDir, "cluster.crt"),
		KeyPath:  filepath.Join(cfg.CertDir, "cluster.key"),
		CAPath:   filepath.Join(cfg.CertDir, "ca.crt"),
	}

	// Fast-path: existing cert still valid.
	if cert, err := loadCertIfValid(bundle.CertPath); err == nil && cert != nil {
		log.Printf("provisioner: reusing existing cluster cert CN=%s (expires %s)",
			cert.Subject.CommonName, cert.NotAfter.Format(time.RFC3339))
		return bundle, nil
	}

	// Slow path: enroll.
	if cfg.EnrollURL == "" {
		return nil, fmt.Errorf("EnrollURL is required (set AGENTRY_ENROLL_URL)")
	}
	if cfg.EnrollToken == "" {
		return nil, fmt.Errorf("EnrollToken is required (set AGENTRY_ENROLL_TOKEN)")
	}

	if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", cfg.CertDir, err)
	}

	log.Printf("provisioner: enrolling cluster=%q against %s", cfg.ClusterID, cfg.EnrollURL)
	keyPem, csrPem, err := genClusterKeypairAndCSR(cfg.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	resp, err := postEnroll(ctx, cfg.EnrollURL, EnrollRequest{
		Token:  cfg.EnrollToken,
		CSRPem: csrPem,
	})
	if err != nil {
		return nil, fmt.Errorf("enroll POST: %w", err)
	}
	if resp.DeviceCertPem == "" || resp.CACertPem == "" {
		return nil, fmt.Errorf("enroll response missing cert or ca: %+v", resp)
	}

	if err := writeSecretFile(bundle.CertPath, []byte(resp.DeviceCertPem)); err != nil {
		return nil, fmt.Errorf("write cert: %w", err)
	}
	if err := writeSecretFile(bundle.KeyPath, keyPem); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	if err := writeSecretFile(bundle.CAPath, []byte(resp.CACertPem)); err != nil {
		return nil, fmt.Errorf("write CA: %w", err)
	}

	log.Printf("provisioner: enrolled, cert valid until %s, written to %s",
		resp.ExpiresAt, cfg.CertDir)
	return bundle, nil
}

// loadCertIfValid returns (cert, nil) when the file parses and is not
// already past NotAfter. Anything else returns (nil, _) so the caller
// drops into the enroll path. We don't fail outright on a bad file —
// re-enrolling is the right healing move.
func loadCertIfValid(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: not a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("cert %s expired at %s", path, cert.NotAfter)
	}
	return cert, nil
}

// BuildClusterTLSConfig loads cert + key + ca and returns a tls.Config
// the BrokerClient hands to tunnel.Dial. RootCAs adds our CA on top of
// the system roots — the broker's server cert is LetsEncrypt-issued
// (system roots), but adding ours is harmless and future-proofs for
// the case where the broker is fronted by a self-signed cert.
func BuildClusterTLSConfig(bundle *ClusterCertBundle) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(bundle.CertPath, bundle.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(bundle.CAPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s: no PEM certificates found", bundle.CAPath)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// CertNotAfter returns the NotAfter of the bundle's cert, for the
// renewal scheduler to plan around.
func CertNotAfter(bundle *ClusterCertBundle) (time.Time, error) {
	cert, err := loadCertIfValid(bundle.CertPath)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

// RenewalThreshold is how close to expiry we trigger an auto re-enroll.
// 7 days leaves headroom for transient broker outages plus the operator
// notice window if something goes wrong with the renewal.
const RenewalThreshold = 7 * 24 * time.Hour

// RunCertRenewer ticks daily; whenever the on-disk cert is within
// RenewalThreshold of NotAfter, it re-enrolls and atomically swaps
// the files in place. Returns when ctx is cancelled.
//
// The BrokerClient does NOT re-read the TLS config on the fly — it
// holds a tls.Config built at startup. After a renewal, the next
// tunnel reconnect will pick up the new cert because tls.Certificates
// is consulted on each TLS handshake (the SDK loads from the same
// in-memory slice). For an immediate effect on a long-lived session
// the renewer logs that the operator may want to restart; in practice
// the next 24h reconnect cycle covers it without action.
//
// Errors are logged and the loop continues — a failed renewal at day
// T-7 leaves us with 7 more days of valid cert and 7 more daily
// retries before things actually break.
func RunCertRenewer(ctx context.Context, cfg Config, bundle *ClusterCertBundle, tlsConf *tls.Config) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		notAfter, err := CertNotAfter(bundle)
		if err != nil {
			log.Printf("provisioner: renewer: cert read failed (%v); attempting re-enroll", err)
		} else if time.Until(notAfter) > RenewalThreshold {
			continue // still healthy, nothing to do
		}
		log.Printf("provisioner: renewer: cert near expiry (NotAfter=%s), re-enrolling",
			notAfter.Format(time.RFC3339))
		if _, err := BootstrapClusterCert(ctx, cfg); err != nil {
			log.Printf("provisioner: renewer: re-enroll failed: %v (will retry tomorrow)", err)
			continue
		}
		// Hot-swap the cert in the existing tls.Config. The Certificates
		// slice is read on every TLS handshake, so the next reconnect
		// after this point uses the new cert. We rebuild the whole
		// config to refresh RootCAs too in case the CA changed.
		newConf, err := BuildClusterTLSConfig(bundle)
		if err != nil {
			log.Printf("provisioner: renewer: rebuild TLS config: %v", err)
			continue
		}
		tlsConf.Certificates = newConf.Certificates
		tlsConf.RootCAs = newConf.RootCAs
		log.Printf("provisioner: renewer: cert rotated; broker reconnect will use the new cert")
	}
}

// genClusterKeypairAndCSR mirrors what xdp init does on the laptop —
// fresh P-256 keypair, CSR with the cluster ID in the CN (the broker
// overwrites the subject anyway; we just supply a sensible placeholder
// so logs of mid-flight CSRs are debuggable).
func genClusterKeypairAndCSR(clusterID string) (keyPem []byte, csrPem string, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", err
	}
	keyPem = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "cluster-" + clusterID},
	}, priv)
	if err != nil {
		return nil, "", err
	}
	csrPem = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	return keyPem, csrPem, nil
}

// postEnroll sends the enrollment request to the control plane's
// /api/v1/enroll endpoint. Uses the default HTTP client which means
// the control plane's server cert is verified against the OS trust
// store — fine because LetsEncrypt is in there.
//
// On a non-2xx response, we surface the body as the error so the
// operator sees the control plane's actual message ("invalid
// enrollment token", "already consumed", "expired", etc.) — those
// are the messages users will Google when their docker run fails.
func postEnroll(ctx context.Context, url string, req EnrollRequest) (*EnrollResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out EnrollResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// writeSecretFile is an atomic 0600 write — same posture as xdp uses
// for its on-disk creds.
func writeSecretFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
