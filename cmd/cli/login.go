package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// cmdLogin is the single entry point that turns a fresh machine into a
// usable agentry install: it mints a PAT, enrolls a device cert, and
// persists both to the local config. The browser flow looks like:
//
//  1. CLI binds a localhost listener on a random high port.
//  2. CLI opens the user's default browser to
//     $APP_URL/cli-login?state=<nonce>&callback=http://127.0.0.1:<port>/cb
//  3. Dashboard forces Clerk sign-in, then renders the approval
//     page. On click it mints a PAT (`POST /api/v1/cli-tokens`)
//     and (in a future revision) enrolls a device cert, then POSTs
//     the result to the callback.
//  4. CLI validates `state`, persists, exits.
//
// The state nonce stops a malicious browser tab from racing the
// callback. The PAT only ever traverses loopback so it can't be
// captured on the network.
func cmdLogin(args []string) int {
	fs := flag.NewFlagSet("agentry login", flag.ContinueOnError)
	appURL := fs.String("app-url", defaultAppURL(), "agentry control plane (e.g. https://app.agentry.run)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser flow to complete")
	force := fs.Bool("force", false, "re-authenticate even if a token is already on disk")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *appURL == "" {
		return die("--app-url is required (or set AGENTRY_APP_URL)")
	}

	// Quick no-op when there's already a working-looking token. Saves a
	// browser round-trip every time a user runs `agentry login` to
	// confirm they're set up. We can't tell server-side if the token's
	// revoked without a network call, so this is a heuristic — pass
	// --force when in doubt.
	if !*force {
		if cfg, _, err := LoadConfig(); err == nil && cfg.APIToken != "" {
			who := emptyAs(cfg.UserEmail, "(unknown user)")
			if cfg.Org != "" {
				who = who + " (" + cfg.Org + ")"
			}
			fmt.Printf("agentry: already logged in as %s\n", who)
			fmt.Println("        pass --force to re-authenticate")
			return 0
		}
	}

	// 16 random bytes — enough that a malicious page can't guess the
	// nonce and POST to our listener pretending to be the auth flow.
	state, err := randomHex(16)
	if err != nil {
		return die("generate state: %v", err)
	}

	// Listen on 127.0.0.1 only. The auth flow MUST round-trip through
	// loopback; we never expose the callback to a real network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return die("bind callback listener: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	loginURL := fmt.Sprintf("%s/cli-login?state=%s&callback=%s",
		strings.TrimRight(*appURL, "/"),
		state,
		fmt.Sprintf("http://127.0.0.1:%d/cb", port),
	)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// resultCh delivers what the browser POSTed back, or an error.
	type cbResult struct {
		Token     string `json:"token"`
		AppURL    string `json:"app_url"`
		OrgName   string `json:"org_name"`
		UserEmail string `json:"user_email"`
		State     string `json:"state"`
	}
	resultCh := make(chan cbResult, 1)
	errCh := make(chan error, 1)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS: the dashboard's origin is the only allowed caller.
		// Browsers preflight POSTs with custom content-type, so we
		// answer OPTIONS too.
		w.Header().Set("Access-Control-Allow-Origin", strings.TrimRight(*appURL, "/"))
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/cb" {
			http.NotFound(w, r)
			return
		}
		var cb cbResult
		if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			errCh <- fmt.Errorf("decode callback: %w", err)
			return
		}
		if cb.State != state {
			http.Error(w, "state mismatch", http.StatusForbidden)
			errCh <- errors.New("state nonce mismatch — abort")
			return
		}
		if cb.Token == "" {
			http.Error(w, "no token", http.StatusBadRequest)
			errCh <- errors.New("no token in callback")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		resultCh <- cb
	})}
	go func() {
		// Serve blocks until ctx ends or the listener closes.
		_ = srv.Serve(ln)
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	fmt.Fprintln(os.Stderr, "agentry: opening browser to authorize…")
	fmt.Fprintln(os.Stderr, "  if it doesn't open, paste this URL manually:")
	fmt.Fprintln(os.Stderr, "  "+loginURL)
	if err := openBrowser(loginURL); err != nil {
		fmt.Fprintln(os.Stderr, "agentry: couldn't auto-open browser:", err)
	}

	select {
	case cb := <-resultCh:
		cfg, _, _ := LoadConfig()
		if cfg == nil {
			cfg = &Config{}
		}
		cfg.AppURL = cb.AppURL
		if cfg.AppURL == "" {
			cfg.AppURL = *appURL
		}
		cfg.APIToken = cb.Token
		cfg.Org = cb.OrgName
		cfg.UserEmail = cb.UserEmail
		if cfg.DeviceID == "" {
			cfg.DeviceID = NewDeviceID()
		}
		if err := cfg.Save(); err != nil {
			return die("save config: %v", err)
		}
		who := cb.UserEmail
		if cb.OrgName != "" {
			who = who + " (" + cb.OrgName + ")"
		}
		fmt.Printf("agentry: logged in as %s\n", who)
		fmt.Println("        run `agentry server` to pick a server, or open https://app.agentry.run to add one")
		return 0

	case err := <-errCh:
		return die("login: %v", err)

	case <-ctx.Done():
		return die("login timed out; re-run `agentry login` and complete the browser flow within %s", *timeout)
	}
}

// cmdLogout drops the local token and revokes it server-side. Best-
// effort on the server call — even if it fails we still wipe the file
// because the user clearly wants out.
func cmdLogout(_ []string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentry: no config to clear")
		return 0
	}
	if cfg.APIToken != "" && cfg.AppURL != "" {
		_ = revokeOwnToken(cfg)
	}
	cfg.APIToken = ""
	cfg.Org = ""
	cfg.UserEmail = ""
	// The server pin is account-scoped — a server belongs to the org you
	// were logged into. Clear it too so a later login/init for another
	// account never inherits a stale selection.
	cfg.Cluster = ""
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	fmt.Println("agentry: logged out")
	return 0
}

// revokeOwnToken hits DELETE /api/v1/cli-tokens/<id> with the token
// itself as auth — the server scopes the delete to the caller, so the
// token can revoke itself but not anyone else's. We extract the id
// from the plain token: `pat_tok_<id>_<secret>`.
func revokeOwnToken(cfg *Config) error {
	parts := strings.Split(cfg.APIToken, "_")
	if len(parts) < 4 || parts[0] != "pat" || parts[1] != "tok" {
		return nil // unknown shape — nothing to do
	}
	tokID := "tok_" + parts[2]
	req, _ := http.NewRequest("DELETE",
		strings.TrimRight(cfg.AppURL, "/")+"/api/v1/cli-tokens/"+tokID, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// defaultAppURL is what `agentry login` targets when --app-url isn't
// passed and no config exists yet. AGENTRY_APP_URL overrides for local
// dev; otherwise it's the prod control plane.
func defaultAppURL() string {
	if v := os.Getenv("AGENTRY_APP_URL"); v != "" {
		return v
	}
	if cfg, _, err := LoadConfig(); err == nil && cfg.AppURL != "" {
		return cfg.AppURL
	}
	return "https://app.agentry.run"
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser shells out to whatever the platform calls "open this
// URL with the default browser". Best-effort; if it fails the user
// pastes the URL by hand (we always print it to stderr first).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
