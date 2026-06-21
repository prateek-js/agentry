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

	"golang.org/x/term"
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
//  1. Take token + app URL (from flags or interactive prompt)
//  2. Generate ECDSA keypair locally — private key never leaves
//  3. Build a CSR carrying the public key
//  4. POST to <app-url>/api/v1/enroll with {token, csr_pem}
//  5. Persist the returned device cert + CA cert + private key
//     alongside ~/.agentry/agentry.json
//
// The token came from the dashboard's "Add this machine" panel and
// is single-use, 1-hour TTL. The cert it issues is 1y; auto-renewal
// would come from a future `agentry refresh` command (deferred).
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("agentry init", flag.ContinueOnError)
	appURL := fs.String("app-url", "", "control-plane URL (e.g. https://app.agentry.run)")
	token := fs.String("token", "", "enrollment token (else prompted)")
	name := fs.String("name", "", "device name (else inferred from hostname)")
	force := fs.Bool("force", false, "replace an existing config without confirming")
	fs.BoolVar(force, "y", false, "alias for --force")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// init sets up this machine from scratch. If a config already exists
	// (e.g. from a previous account), replacing it is destructive — it
	// drops the old login, server pin, and device cert. Confirm first so
	// switching accounts on the same machine never silently inherits the
	// old account's stale server selection.
	if existing, path, err := LoadConfig(); err == nil && existing.configured() && !*force {
		if !confirmReplace(existing, path) {
			fmt.Fprintln(os.Stderr, "agentry: init aborted; existing config left unchanged")
			return 1
		}
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

	// Fresh config. init sets up THIS machine from scratch: we never
	// inherit a previous account's PAT, org, or server pin — carrying
	// those across an account switch is exactly the cross-account
	// staleness this command is meant to clear (the user confirmed the
	// replace above). The user re-runs `agentry login` for the new
	// account afterward.
	cfg := &Config{
		AppURL:         strings.TrimRight(*appURL, "/"),
		BrokerURL:      resp.BridgeURL,
		DeviceID:       deviceName,
		DeviceCertPath: certPath,
		DeviceKeyPath:  keyPath,
		CACertPath:     caPath,
	}
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}

	fmt.Printf("agentry config written to %s\n", ConfigPath())
	fmt.Printf("  app:     %s\n", cfg.AppURL)
	fmt.Printf("  bridge:  %s\n", cfg.BrokerURL)
	fmt.Printf("  device:  %s\n", cfg.DeviceID)
	fmt.Printf("  cert:    %s (valid until %s)\n", cfg.DeviceCertPath, resp.ExpiresAt)
	fmt.Println()
	fmt.Println("Next: run `agentry login` to sign in, `agentry server` to pick a target, then point your AI client at `agentry mcp`.")
	return 0
}

// confirmReplace warns that init will discard an existing config and
// asks for a y/N confirmation. On a non-interactive stdin it refuses
// (returns false) rather than silently clobbering — the caller can pass
// --force for the headless path.
func confirmReplace(existing *Config, path string) bool {
	fmt.Fprintf(os.Stderr, "An agentry config already exists at %s:\n", path)
	if existing.UserEmail != "" {
		fmt.Fprintf(os.Stderr, "  account: %s\n", existing.UserEmail)
	}
	if existing.DeviceID != "" {
		fmt.Fprintf(os.Stderr, "  device:  %s\n", existing.DeviceID)
	}
	if existing.Cluster != "" {
		fmt.Fprintf(os.Stderr, "  server:  %s\n", existing.Cluster)
	}
	fmt.Fprintln(os.Stderr, "Continuing will REPLACE it with a fresh setup — the old login, server pin, and device cert are discarded.")
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "Re-run with --force to replace it non-interactively.")
		return false
	}
	ans := strings.ToLower(prompt("Replace it? [y/N]: "))
	return ans == "y" || ans == "yes"
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
