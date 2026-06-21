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

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"golang.org/x/crypto/acme/autocert"

	"github.com/agentry-ai/agentry/pkg/bridge"
)

func main() {
	addr := flag.String("listen", ":8090", "(DEV_MODE only) plain-HTTP bind address")
	flag.Parse()

	env := loadEnv()

	// Sentry — opt-in via SENTRY_DSN. Silent when unset (local / dev).
	// When set, panics in request handling + explicit captures flow out,
	// each tagged with the bridge environment + release. Flushed on the
	// graceful-shutdown path below (Fatalf paths skip it by design).
	if sentryDSN := os.Getenv("SENTRY_DSN"); sentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              sentryDSN,
			Environment:      envOr("SENTRY_ENV", "production"),
			Release:          envOr("SENTRY_RELEASE", "agentry-bridge@dev"),
			TracesSampleRate: 0.1,
		}); err != nil {
			log.Printf("bridge: sentry init: %v (continuing without)", err)
		} else {
			defer sentry.Flush(2 * time.Second)
		}
	}

	// DEV_MODE turns off mTLS, role-CN binding, org-SAN tenancy, the
	// cross-org routing check, and the admin gate on the route table —
	// it makes the bridge a fully open routing pivot. That's fine for
	// local dev, but a single stray DEV_MODE=true in production would
	// silently collapse the entire zero-trust model. Refuse to start if
	// DEV_MODE is on while a production domain is configured.
	if env.devMode && (env.tlsDomain != "" || env.deployDomain != "") {
		log.Fatalf("bridge: refusing to start — DEV_MODE disables ALL mTLS/role/org checks, "+
			"but a production domain is configured (tls=%q deploy=%q). "+
			"Unset DEV_MODE for production, or unset the domains for local dev.",
			env.tlsDomain, env.deployDomain)
	}

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
		Handler:           sentryWrap(b.Handler()),
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

	// Optional: wildcard cert for *.<deployDomain> via Cloudflare DNS-01.
	// Returns nil when CF_API_TOKEN isn't set; everything else here just
	// degrades to "no deployment traffic accepted."
	deployCerts := newDeployCertManager(env.cfAPIToken, env.deployCertCacheDir, env.deployDomain)
	if deployCerts != nil && env.deployDomain != "" {
		go func() {
			if err := deployCerts.ensureWildcard(env.deployDomain); err != nil {
				log.Printf("bridge: deploy wildcard cert provisioning failed: %v", err)
			}
		}()
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	// Deploy registry created here (before the TLS config) so the SNI
	// dispatcher can recognise registered custom-domain hosts and present
	// a cert for them. Routes are pushed in via /api/deploy-routes.
	deployReg := bridge.NewDeployRegistry()
	b.AttachDeploy(deployReg)

	tlsConf := &tls.Config{
		// SNI dispatcher: deployment hostnames under the deploy domain
		// get the wildcard cert; registered custom domains (BYO) get the
		// wildcard too (CF's origin pull doesn't validate the cert name —
		// see wildcardCertFor); everything else (bridge.agentry.run)
		// falls through to the HTTP-01 autocert manager.
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni := hello.ServerName
			if env.deployDomain != "" && hostMatchesDeployDomain(sni, env.deployDomain) {
				return deployCerts.getCertificate(hello)
			}
			if deployCerts != nil {
				if _, ok := deployReg.Lookup(sni); ok {
					return deployCerts.wildcardCertFor(hello, env.deployDomain)
				}
			}
			return mgr.GetCertificate(hello)
		},
		// VerifyClientCertIfGiven: TLS layer validates a cert if the
		// client offers one. mtlsGate below enforces "you MUST offer
		// one" on every non-exempt path. Browser deployment traffic
		// won't present a cert; that's fine here, the handler dispatch
		// below picks the auth path by Host header.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  caPool,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1", "acme-tls/1"},
	}

	// (deployReg created above, before tlsConf, so the SNI dispatcher
	// can see registered custom-domain hosts.)

	// Handler dispatch:
	//   - hostnames under deployDomain that have a registered route →
	//     org-mode auth gate, then reverse-proxy through the cluster tunnel
	//   - hostnames under deployDomain with no route → static placeholder
	//   - everything else (bridge.agentry.run) → existing mTLS-gated
	//     broker handler
	mainHandler := mtlsGate(b, b.Handler())
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.Index(host, ":"); i > 0 {
			host = host[:i]
		}
		// A request is deploy traffic if either (a) it's under our deploy
		// domain (*.agentry.live), or (b) it's a custom domain the control
		// plane has registered a route for. Lookup is the allowlist: a
		// random Host with no registered route still falls through to the
		// mTLS-gated bridge API and gets rejected (default-deny). This is
		// what lets bring-your-own-domain hosts (app.customer.com) reach
		// HandleDeployment even though they aren't under agentry.live.
		route, ok := deployReg.Lookup(host)
		underDeployDomain := env.deployDomain != "" && hostMatchesDeployDomain(r.Host, env.deployDomain)
		if ok || underDeployDomain {
			if !ok {
				// Under the deploy domain but no route registered yet →
				// the friendly placeholder. (Unregistered custom domains
				// never reach here; they fall to the API path below.)
				deploymentPlaceholder(w, r, env.deployDomain)
				return
			}
			// Dispatch by auth mode. Password mode short-circuits to its
			// own form/cookie flow; org mode goes through the Clerk
			// handoff; public falls through to the upstream proxy.
			switch route.AuthMode {
			case "password":
				if !checkDeployAuthPassword(w, r, route, env.deployAuthSecret) {
					return
				}
			default:
				if !checkDeployAuth(w, r, route, env.deployAuthSecret, env.appURL) {
					return // checkDeployAuth wrote the response (redirect or 4xx)
				}
			}
			b.HandleDeployment(w, r)
			return
		}
		mainHandler.ServeHTTP(w, r)
	})

	httpsSrv := &http.Server{
		Addr:              env.httpsListen,
		Handler:           sentryWrap(dispatcher),
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 30 * time.Second,
		// No ReadTimeout/WriteTimeout — once handleTunnel hijacks, the
		// session is owned by yamux for hours, and deploy responses can
		// stream. ReadHeaderTimeout already bounds slow-header slowloris;
		// IdleTimeout reaps idle keep-alive conns (it does NOT apply to a
		// hijacked tunnel, so it's safe here).
		IdleTimeout: 120 * time.Second,
	}

	httpSrv := &http.Server{
		Addr:    env.httpListen,
		Handler: mgr.HTTPHandler(http.HandlerFunc(redirectToHTTPS(env.tlsDomain))),
		// :80 only serves ACME challenges + redirects — nothing hijacks,
		// nothing streams, so it gets full slow-client timeouts.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
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
func mtlsGate(b *bridge.Broker, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			inner.ServeHTTP(w, r)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		// Revocation: a deleted cluster/device's cert is valid (CA-signed,
		// unexpired) but must no longer be trusted. The control plane
		// pushes revoked CNs; reject the handshake before it reaches any
		// handler. New tunnels are blocked immediately; an already-open
		// session rides until it drops.
		if b.IsRevoked(r.TLS.PeerCertificates[0].Subject.CommonName) {
			http.Error(w, "client certificate revoked", http.StatusForbidden)
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

	// Deployment ingress. When set, *.<deployDomain> traffic flows
	// through the bridge with a wildcard cert provisioned via Cloudflare
	// DNS-01. Without these the bridge keeps doing only its existing
	// mTLS-gated work on tlsDomain.
	deployDomain       string
	deployCertCacheDir string
	cfAPIToken         string

	// Org-mode deploy auth. Bridge enforces "you must be signed into
	// the route's org" by bouncing browser traffic to a handoff
	// endpoint on appURL, which mints HMAC-signed tokens with this
	// secret. Both knobs unset = org-mode routes 503 (fail closed).
	deployAuthSecret []byte
	appURL           string
}

func loadEnv() envConfig {
	cfg := envConfig{
		devMode:            envBool("DEV_MODE"),
		tlsDomain:          os.Getenv("TLS_DOMAIN"),
		tlsCacheDir:        os.Getenv("TLS_CACHE_DIR"),
		httpsListen:        os.Getenv("HTTPS_LISTEN"),
		httpListen:         os.Getenv("HTTP_LISTEN"),
		caCertURL:          os.Getenv("CA_CERT_URL"),
		caCertPath:         os.Getenv("CA_CERT_PATH"),
		deployDomain:       os.Getenv("DEPLOY_DOMAIN"),
		deployCertCacheDir: os.Getenv("DEPLOY_CERT_CACHE_DIR"),
		cfAPIToken:         os.Getenv("CF_API_TOKEN"),
		appURL:             os.Getenv("APP_URL"),
	}
	if cfg.appURL == "" {
		cfg.appURL = "https://app.agentry.run"
	}
	if hex := os.Getenv("AGENTRY_DEPLOY_HANDOFF_SECRET"); hex != "" {
		secret, err := decodeHexSecret(hex)
		if err != nil {
			log.Fatalf("bridge: AGENTRY_DEPLOY_HANDOFF_SECRET parse: %v", err)
		}
		cfg.deployAuthSecret = secret
	}
	if cfg.deployCertCacheDir == "" {
		cfg.deployCertCacheDir = "/var/lib/agentry-bridge/deploy-certs"
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sentryWrap adds the Sentry HTTP middleware (panic capture + per-request
// context) around a handler. A no-op when SENTRY_DSN was unset, since
// sentry.Init was never called. Repanic:true re-raises after capture so
// the http.Server's own per-connection recovery still closes the conn.
func sentryWrap(h http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(h)
}
