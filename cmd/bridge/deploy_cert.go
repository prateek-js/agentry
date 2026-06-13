package main

import (
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// deployCertManager is the cert source for *.agentry.live deployment
// URLs. Uses certmagic with a Cloudflare DNS-01 solver so we can hold
// a single wildcard cert instead of provisioning a fresh cert per
// deployment. DNS-only — Cloudflare proxy is never in the TLS path.
//
// Cache lives on disk under certCacheDir so cluster restart doesn't
// trigger a re-issue (LetsEncrypt rate-limits to 50 new orders/week).
//
// Returns nil when no CF token is configured — caller falls back to
// the existing HTTP-01 autocert path for legacy bridge.agentry.run
// traffic.
type deployCertManager struct {
	cfg *certmagic.Config
}

func newDeployCertManager(cfAPIToken, cacheDir, deployDomain string) *deployCertManager {
	if cfAPIToken == "" {
		log.Print("bridge: CF_API_TOKEN unset — deployment wildcard cert disabled")
		return nil
	}
	// CertMagic config tuned for one wildcard.
	storage := &certmagic.FileStorage{Path: cacheDir}
	cfg := certmagic.NewDefault()
	cfg.Storage = storage
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		Agreed:                  true,
		Email:                   "admin@agentry.run",
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{APIToken: cfAPIToken},
			},
		},
	})
	cfg.Issuers = []certmagic.Issuer{issuer}
	return &deployCertManager{cfg: cfg}
}

// ensureWildcard kicks off cert provisioning for *.<domain>. Runs once
// at bridge startup. CertMagic refreshes in the background; no manual
// renewal needed.
func (d *deployCertManager) ensureWildcard(domain string) error {
	if d == nil {
		return nil
	}
	wildcard := "*." + domain
	log.Printf("bridge: provisioning wildcard cert for %s via Cloudflare DNS-01", wildcard)
	return d.cfg.ManageAsync(nil, []string{wildcard})
}

// getCertificate is the SNI dispatcher's call into certmagic for
// hostnames matching our deployment domain. Other hostnames fall
// through to the legacy autocert manager (bridge.agentry.run).
func (d *deployCertManager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if d == nil {
		return nil, errors.New("deploy cert manager not configured")
	}
	return d.cfg.GetCertificate(hello)
}

// wildcardCertFor returns the *.<deployDomain> cert regardless of the
// client's SNI. Used for bring-your-own custom-domain origin pulls:
// Cloudflare for SaaS terminates the customer's real cert at its edge,
// then connects to us (the fallback origin) with SNI = the custom
// hostname (e.g. me.wina.sh) — a name we can't hold a cert for. CF's
// origin pull doesn't validate the cert name, so presenting the wildcard
// just lets the handshake complete; routing still happens by Host. We
// copy the hello and only rewrite ServerName so cipher/version
// negotiation is unchanged.
func (d *deployCertManager) wildcardCertFor(hello *tls.ClientHelloInfo, deployDomain string) (*tls.Certificate, error) {
	if d == nil {
		return nil, errors.New("deploy cert manager not configured")
	}
	h := *hello
	h.ServerName = "origin." + deployDomain
	return d.cfg.GetCertificate(&h)
}

// hostMatchesDeployDomain returns true for hosts under the deployment
// apex (e.g. "sales-dash-abc.agentry.live" when deployDomain is
// "agentry.live"). The apex itself doesn't match — only subdomains.
// Strips ":port" suffix that Go sometimes leaves on r.Host.
func hostMatchesDeployDomain(host, deployDomain string) bool {
	if i := strings.Index(host, ":"); i > 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	deployDomain = strings.ToLower(deployDomain)
	if host == deployDomain {
		return false
	}
	return strings.HasSuffix(host, "."+deployDomain)
}

// deploymentPlaceholder is the temporary handler for deployment
// hostnames. Returns a small HTML page so a browser hitting
// <whatever>.<deployDomain> proves the wildcard cert + Host routing
// are working end-to-end. Gets swapped for the real Clerk-gated
// tunnel proxy in the next slice of this batch.
func deploymentPlaceholder(w http.ResponseWriter, r *http.Request, deployDomain string) {
	host := r.Host
	if i := strings.Index(host, ":"); i > 0 {
		host = host[:i]
	}
	sub := strings.TrimSuffix(host, "."+deployDomain)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8>
<title>agentry.live</title>
<style>
  body{font-family:system-ui;background:#fafafa;color:#333;
       display:grid;place-items:center;min-height:100vh;margin:0}
  .card{background:white;padding:3rem;border-radius:1rem;
        box-shadow:0 2px 20px rgba(0,0,0,.06);text-align:center;max-width:32rem}
  code{background:#f3f3f3;padding:.2rem .4rem;border-radius:.3rem;font-size:.9rem}
  .subdomain{font-family:monospace;color:#16a34a;font-weight:600}
</style>
<div class=card>
  <h1>agentry.live</h1>
  <p>Wildcard TLS is live. You hit <span class=subdomain>` + sub + `</span> but
     no deployment is bound to this hostname yet.</p>
  <p>This page is the placeholder while the deployment routing slice
     ships. Once it lands, sandbox URLs allocated by
     <code>sandbox_create</code> serve here automatically.</p>
</div>`))
}
