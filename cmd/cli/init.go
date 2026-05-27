package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// EnrollRequest is the wire shape the control plane's /api/v1/enroll
// endpoint accepts. Inlined here so the CLI doesn't depend on the
// closed-source agentry-app module.
type EnrollRequest struct {
	Token  string `json:"token"`
	CSRPem string `json:"csr_pem"`
}

// EnrollResponse mirrors agentry-app's response. BridgeURL is the
// dial target subsequent commands write into the config so the user
// never has to know where the bridge lives.
type EnrollResponse struct {
	DeviceCertPem string `json:"device_cert_pem"`
	CACertPem     string `json:"ca_cert_pem"`
	BridgeURL     string `json:"bridge_url"`
	ExpiresAt     string `json:"expires_at"`
}

// cmdInit handles first-touch onboarding for a laptop:
//
//   1. Take token + app URL (from flags or interactive prompt)
//   2. Generate ECDSA keypair locally — private key never leaves
//   3. Build a CSR carrying the public key
//   4. POST to <app-url>/api/v1/enroll with {token, csr_pem}
//   5. Persist the returned device cert + CA cert + private key
//      alongside ~/.agentry/agentry.json
//
// The token came from the dashboard's "Add this machine" panel and
// is single-use, 1-hour TTL. The cert it issues is 1y; auto-renewal
// would come from a future `agentry refresh` command (deferred).
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("agentry init", flag.ContinueOnError)
	appURL := fs.String("app-url", "", "control-plane URL (e.g. https://app.agentry.run)")
	token := fs.String("token", "", "enrollment token (else prompted)")
	name := fs.String("name", "", "device name (else inferred from hostname)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *appURL == "" {
		*appURL = os.Getenv("AGENTRY_APP_URL")
	}
	if *appURL == "" {
		*appURL = prompt("agentry app URL (e.g. https://app.agentry.run): ")
	}
	if *appURL == "" {
		return die("agentry app URL is required (--app-url or AGENTRY_APP_URL)")
	}
	if *token == "" {
		*token = prompt("enrollment token: ")
	}
	if *token == "" {
		return die("enrollment token is required (--token or paste at prompt)")
	}
	enrollURL := strings.TrimRight(*appURL, "/") + "/api/v1/enroll"

	deviceName := *name
	if deviceName == "" {
		host, _ := os.Hostname()
		deviceName = strings.ToLower(strings.Split(host, ".")[0])
		if deviceName == "" {
			deviceName = "laptop"
		}
	}

	// Generate keypair + CSR. The CSR's subject is overwritten by the
	// control plane (it pins CN = device-<user>-<name>); we set a
	// placeholder here just so mid-flight log lines are debuggable.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return die("generate keypair: %v", err)
	}
	keyPEM, err := encodePrivateKey(priv)
	if err != nil {
		return die("encode key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "device-" + deviceName},
	}, priv)
	if err != nil {
		return die("build CSR: %v", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	resp, err := postEnroll(enrollURL, EnrollRequest{Token: *token, CSRPem: csrPEM})
	if err != nil {
		return die("enroll: %v", err)
	}
	if resp.DeviceCertPem == "" || resp.CACertPem == "" {
		return die("enroll response missing cert or ca")
	}

	// Persist to ~/.agentry/. Same posture cert + key files use: dir
	// 0700, files 0600.
	dir := filepath.Dir(ConfigPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return die("mkdir %s: %v", dir, err)
	}
	certPath := filepath.Join(dir, "device.crt")
	keyPath := filepath.Join(dir, "device.key")
	caPath := filepath.Join(dir, "ca.crt")
	if err := writeSecret(certPath, []byte(resp.DeviceCertPem)); err != nil {
		return die("write cert: %v", err)
	}
	if err := writeSecret(keyPath, keyPEM); err != nil {
		return die("write key: %v", err)
	}
	if err := writeSecret(caPath, []byte(resp.CACertPem)); err != nil {
		return die("write CA: %v", err)
	}

	// Preserve cluster selection across re-init so the user doesn't
	// lose their current target.
	existing, _, _ := LoadConfig()
	cluster := ""
	if existing != nil {
		cluster = existing.Cluster
	}

	cfg := &Config{
		AppURL:         strings.TrimRight(*appURL, "/"),
		BrokerURL:      resp.BridgeURL,
		DeviceID:       deviceName,
		DeviceCertPath: certPath,
		DeviceKeyPath:  keyPath,
		CACertPath:     caPath,
		Cluster:        cluster,
	}
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}

	fmt.Printf("agentry config written to %s\n", ConfigPath())
	fmt.Printf("  app:     %s\n", cfg.AppURL)
	fmt.Printf("  bridge:  %s\n", cfg.BrokerURL)
	fmt.Printf("  device:  %s\n", cfg.DeviceID)
	fmt.Printf("  cert:    %s (valid until %s)\n", cfg.DeviceCertPath, resp.ExpiresAt)
	if cluster == "" {
		fmt.Println()
		fmt.Println("Next: run `agentry cluster` to pick a target cluster, then point your AI client at `agentry stdio`.")
	} else {
		fmt.Printf("  cluster: %s\n", cluster)
	}
	return 0
}

// encodePrivateKey writes the ECDSA private key in PKCS#8 PEM form —
// what tls.LoadX509KeyPair expects.
func encodePrivateKey(priv *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// writeSecret is the atomic 0600 write used for both the cert and the
// private key files. Same pattern Config.Save uses.
func writeSecret(path string, data []byte) error {
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

// postEnroll is the one HTTP call agentry init makes. The control
// plane's server cert is verified against the OS trust store
// (LetsEncrypt in prod, mkcert in dev). No mTLS yet at this point —
// we don't have a cert; the token IS the credential.
func postEnroll(url string, req EnrollRequest) (*EnrollResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", url, err)
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

// prompt reads a line from stdin with `label` printed to stderr.
// Used for the interactive token / URL prompts when flags weren't
// provided.
func prompt(label string) string {
	fmt.Fprint(os.Stderr, label)
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if n == 0 || err != nil {
			break
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
	}
	return strings.TrimRight(string(buf), " \t\r\n")
}
