// agentry-bridge is the stateless mTLS/yamux routing pivot.
//
// Devices (laptops running the agentry CLI) and clusters (provisioners
// on user infrastructure) both dial in over outbound HTTPS. The bridge
// forwards each request to the cluster named in the X-Cluster header,
// then byte-pipes the rest of the session.
//
// Two operational modes:
//
//   - DEV_MODE=true (local development): plain HTTP on the listen
//     address. No autocert, no mTLS, no role-CN check. Everything is
//     open. Strictly for local-loop testing.
//
//   - DEV_MODE=false (production): HTTPS only. LetsEncrypt provides
//     the server cert via golang.org/x/crypto/acme/autocert. The CA
//     used to validate client certs is fetched from the control plane
//     (CA_CERT_URL) or loaded from disk (CA_CERT_PATH). /tunnel and
//     /api/clusters require a valid client cert; /healthz is open.
//
// Cert issuance lives in the control plane (app.agentry.run), not
// here. The bridge has no KMS access, no database, no Postgres — it's
// a pure stateless routing pivot.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/agentry/agentry/pkg/bridge"
)

func main() {
	addr := flag.String("listen", ":8090", "(DEV_MODE only) plain-HTTP bind address")
	flag.Parse()

	env := loadEnv()

	var caCert *x509.Certificate
	if !env.devMode {
		var err error
		caCert, err = loadCA(env)
		if err != nil {
			log.Fatalf("bridge: load CA: %v", err)
		}
	}

	b := bridge.NewWithConfig(bridge.Config{DevMode: env.devMode})

	if env.devMode {
		log.Printf("bridge: DEV_MODE on — plain HTTP on %s, no mTLS, no role-CN checks", *addr)
	} else {
		log.Printf("bridge: PRODUCTION — domain=%s, CA loaded (CN=%s)",
			env.tlsDomain, caCert.Subject.CommonName)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	if env.devMode {
		runPlain(b, *addr, stop)
	} else {
		runTLS(b, env, caCert, stop)
	}
}

// runPlain is the dev path: plain HTTP on the legacy -listen flag.
func runPlain(b *bridge.Broker, addr string, stop <-chan os.Signal) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           b.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		log.Printf("bridge listening on %s (dev / plain HTTP)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("bridge: %v", err)
		}
	}()
	<-stop
	shutdown(b, srv, nil)
}

// runTLS is the production path. Two servers:
//
//   - :80 — autocert HTTP-01 challenges. Everything else gets a 301
//     to the canonical https host.
//
//   - :443 — main API. Server cert from autocert. mTLS via
//     VerifyClientCertIfGiven; a wrapping middleware demands a
//     verified peer cert for every path except /healthz.
func runTLS(b *bridge.Broker, env envConfig, caCert *x509.Certificate, stop <-chan os.Signal) {
	mgr := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(env.tlsDomain),
		Cache:      autocert.DirCache(env.tlsCacheDir),
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	tlsConf := &tls.Config{
		GetCertificate: mgr.GetCertificate,
		// VerifyClientCertIfGiven: TLS layer validates a cert if the
		// client offers one. mtlsGate below enforces "you MUST offer
		// one" on every non-exempt path.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  caPool,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1", "acme-tls/1"},
	}

	httpsSrv := &http.Server{
		Addr:              env.httpsListen,
		Handler:           mtlsGate(b.Handler()),
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 30 * time.Second,
		// No ReadTimeout/WriteTimeout — once handleTunnel hijacks, the
		// session is owned by yamux for hours.
	}

	httpSrv := &http.Server{
		Addr:              env.httpListen,
		Handler:           mgr.HTTPHandler(http.HandlerFunc(redirectToHTTPS(env.tlsDomain))),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("bridge HTTPS listening on %s", env.httpsListen)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("bridge HTTPS: %v", err)
		}
	}()
	go func() {
		log.Printf("bridge HTTP redirector on %s", env.httpListen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("bridge HTTP: %v", err)
		}
	}()

	<-stop
	shutdown(b, httpsSrv, httpSrv)
}

// mtlsGate turns VerifyClientCertIfGiven into "require client cert
// except where we explicitly opt out". Today the only exempt path is
// /healthz (so load balancers can probe without a client cert).
//
// Cert issuance used to live behind /api/discovery on the broker; in
// agentry that endpoint moves to app.agentry.run, so the bridge no
// longer needs to leave it exempt.
func mtlsGate(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			inner.ServeHTTP(w, r)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// redirectToHTTPS turns plain :80 traffic into a 301 toward the same
// path on the canonical https host. Used as the fallback handler
// behind autocert's HTTPHandler (which handles the ACME paths).
func redirectToHTTPS(domain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + domain + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}

// loadCA resolves the bridge's trust anchor. Preference order:
//
//  1. CA_CERT_URL — fetched once at startup over HTTPS. Public material
//     (the CA *cert*, not the private key), so no auth needed. Lets the
//     bridge pick up CA rotations automatically on restart.
//  2. CA_CERT_PATH — file on disk. Used when the control plane isn't
//     reachable at bridge startup, or when an operator wants pinned
//     immutable trust.
//
// At least one must be set in prod mode.
func loadCA(env envConfig) (*x509.Certificate, error) {
	if env.caCertURL != "" {
		log.Printf("bridge: fetching CA cert from %s", env.caCertURL)
		return fetchCACert(env.caCertURL)
	}
	if env.caCertPath != "" {
		log.Printf("bridge: loading CA cert from %s", env.caCertPath)
		return readCACert(env.caCertPath)
	}
	return nil, fmt.Errorf("either CA_CERT_URL or CA_CERT_PATH must be set in prod mode")
}

func fetchCACert(url string) (*x509.Certificate, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseCACert(raw, url)
}

func readCACert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCACert(raw, path)
}

func parseCACert(raw []byte, source string) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: not a PEM CERTIFICATE", source)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	return cert, nil
}

// shutdown drains the bridge session table + closes the http server(s)
// within a bounded window. Best-effort — SIGTERM should never hang the
// process.
func shutdown(b *bridge.Broker, srvA, srvB *http.Server) {
	log.Print("bridge: shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.Shutdown(ctx)
	if srvA != nil {
		_ = srvA.Shutdown(ctx)
	}
	if srvB != nil {
		_ = srvB.Shutdown(ctx)
	}
}

// envConfig collects every env-var-driven knob so loadEnv has one
// return type.
type envConfig struct {
	devMode bool

	tlsDomain   string
	tlsCacheDir string
	httpsListen string
	httpListen  string

	caCertURL  string
	caCertPath string
}

func loadEnv() envConfig {
	cfg := envConfig{
		devMode:     envBool("DEV_MODE"),
		tlsDomain:   os.Getenv("TLS_DOMAIN"),
		tlsCacheDir: os.Getenv("TLS_CACHE_DIR"),
		httpsListen: os.Getenv("HTTPS_LISTEN"),
		httpListen:  os.Getenv("HTTP_LISTEN"),
		caCertURL:   os.Getenv("CA_CERT_URL"),
		caCertPath:  os.Getenv("CA_CERT_PATH"),
	}
	if cfg.httpsListen == "" {
		cfg.httpsListen = ":443"
	}
	if cfg.httpListen == "" {
		cfg.httpListen = ":80"
	}
	if cfg.tlsCacheDir == "" {
		cfg.tlsCacheDir = "/var/lib/agentry-bridge/autocert"
	}
	return cfg
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
