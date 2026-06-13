package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// main.go — entrypoint + signal handling.
//
// Boot sequence:
//   1. loadConfig() — refuses to start in a misconfigured auth mode.
//   2. openStore() — runs the schema migration; refuses to start if
//      the DB is unreachable. Same hard-failure principle as
//      `agentry auth enable`: if auth is on, it works or we crash.
//   3. Build the mux, register handlers, start ListenAndServe.
//   4. Catch SIGTERM/SIGINT, shutdown cleanly within 15s.
//
// Mode=passthrough takes a fast path that doesn't open the DB or
// register the auth surface — we're just a proxy.

func main() {
	code, err := run()
	if err != nil {
		log.Fatalf("authproxy: %v", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

// run returns (exit-code, fatal-err). A non-nil err is a startup
// failure (log + exit 1 via log.Fatalf). A non-zero code with nil err
// is the child process's exit status in exec mode — main propagates
// it via os.Exit so the container surface matches the user app.
func run() (int, error) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := loadConfig()
	if err != nil {
		return 1, fmt.Errorf("config: %w", err)
	}
	if cfg.Debug {
		log.Printf("authproxy: config: mode=%s db=%s upstream=%s port=%s providers=%s",
			cfg.Mode, cfg.DBKind, cfg.Upstream, cfg.Port, providerNames(cfg))
	}

	// In exec mode we spawn the child FIRST so its PORT setup is in
	// place before the mux is built — buildMux() reads cfg.Upstream,
	// which startChild rewrites to the loopback at the shifted port.
	var teardownStore func()
	if execModeEnabled() {
		// Build the listener AFTER the child starts (so Upstream is
		// rewritten). runExecMode handles signal forwarding +
		// child-supervision; we hand it a closure that constructs +
		// runs the http.Server.
		listen := func() error {
			mux, td, err := buildMux(cfg)
			if err != nil {
				return err
			}
			teardownStore = td
			srv := newHTTPServer(cfg, mux)
			log.Printf("authproxy: listening on :%s", cfg.Port)
			return srv.ListenAndServe()
		}
		// Need the child to come up first.
		code := runExecMode(cfg, listen)
		if teardownStore != nil {
			teardownStore()
		}
		return code, nil
	}

	mux, teardown, err := buildMux(cfg)
	if err != nil {
		return 1, err
	}
	if teardown != nil {
		defer teardown()
	}

	srv := newHTTPServer(cfg, mux)

	// Signal-handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("authproxy: listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		log.Printf("authproxy: shutdown signal received")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return 1, fmt.Errorf("shutdown: %w", err)
		}
		return 0, nil
	case err := <-errCh:
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
}

// buildMux constructs the routing tree based on cfg.Mode. Returns the
// mux + an optional teardown (Store.Close) callable the caller defers.
func buildMux(cfg *Config) (*http.ServeMux, func(), error) {
	mux := http.NewServeMux()
	switch cfg.Mode {
	case "passthrough":
		log.Printf("authproxy: passthrough mode — forwarding all traffic to %s", cfg.Upstream)
		mux.Handle("/", proxyHandler(cfg, ""))
		return mux, nil, nil
	case "agentry":
		store, err := openStore(cfg.DBKind, cfg.DBURL)
		if err != nil {
			return nil, nil, fmt.Errorf("open store: %w", err)
		}
		teardown := func() {
			if err := store.Close(); err != nil {
				log.Printf("authproxy: store.Close: %v", err)
			}
		}
		// Mailer is nil unless an SMTP service is bound — that nil is the
		// "email capability off" state the handlers + login page key off.
		var mailer Mailer
		if cfg.EmailEnabled() {
			mailer = newSMTPMailer(cfg.Email)
		}
		auth := &authHandlers{
			cfg:     cfg,
			store:   store,
			mailer:  mailer,
			limiter: newRateLimiter(loginRateMax, loginRateWindow),
		}
		oauth := &oauthHandlers{cfg: cfg, store: store}
		// Order matters: oauth's per-provider routes must be
		// registered BEFORE auth.register, because auth.register
		// installs a catch-all on "/" that would shadow them.
		oauth.register(mux)
		auth.register(mux, proxyHandler(cfg, cfg.Secret))
		log.Printf("authproxy: auth mode — db=%s upstream=%s providers=%s",
			cfg.DBKind, cfg.Upstream, providerNames(cfg))
		return mux, teardown, nil
	default:
		return nil, nil, fmt.Errorf("unknown mode %q", cfg.Mode)
	}
}

func newHTTPServer(cfg *Config, mux http.Handler) *http.Server {
	return &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      logMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

// providerNames flattens the configured provider names into a stable
// comma list for the boot log.
func providerNames(cfg *Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

// logMiddleware emits a single line per request. Format:
//
//	<METHOD> <PATH> <STATUS> <duration> ip=<ip>
//
// Plain-text on purpose — when something is wrong with auth, the
// operator's first move is `docker logs` and grep, and JSON would just
// slow that down. Production gets the same line via the bridge's
// stdout aggregator.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s ip=%s",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Microsecond),
			remoteIP(r))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Compile-time guard: os.Stderr is what log.Fatalf writes to. Keep an
// explicit reference so a future "drop the log package for slog" change
// doesn't accidentally orphan the stderr binding.
var _ = os.Stderr
