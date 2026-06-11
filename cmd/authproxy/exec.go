package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// exec.go — process-supervisor mode.
//
// When AGENTRY_AUTHPROXY_EXEC is set, the sidecar doesn't expect the
// user's app to be already running on the upstream port. Instead, it
// SPAWNS the app as a child process with a shifted PORT, then runs
// its own listener in the parent.
//
// The wire model:
//
//	external --(:PORT)-->  authproxy  --(http://127.0.0.1:UPSTREAM_PORT)-->  user app
//
// PORT is what the bridge / runtime expects (3000 by convention).
// UPSTREAM_PORT is PORT+1 unless overridden via
// AGENTRY_AUTHPROXY_UPSTREAM_PORT.
//
// This means a single image entrypoint is `authproxy`, and the user's
// app is described via AGENTRY_AUTHPROXY_EXEC=<command>. No
// supervisord, no shell wrappers, no second process for systemd /
// runit to manage. tini stays PID 1; authproxy is PID 2; user's app
// is PID 3 in a separate process group so we can SIGTERM the whole
// tree cleanly.
//
// Signal/lifecycle rules (chosen so a stuck app can't outlive a
// container shutdown):
//
//   - child dies → authproxy logs + exits with child's status
//   - SIGTERM / SIGINT from above → forward to child PGID; wait 15s;
//     SIGKILL on hangouts
//   - listener dies → SIGTERM child, then exit
//
// Implementation detail: we set Setpgid so signalling the *child's
// pgid* doesn't also signal authproxy (we're in our own pgid).

// execModeEnabled returns true when AGENTRY_AUTHPROXY_EXEC is set.
// The env var carries the user's command, space-separated. For
// commands with shell features (env interpolation, &&, redirects),
// wrap with `sh -c '<cmd>'` so this stays a literal argv vector.
func execModeEnabled() bool {
	return strings.TrimSpace(os.Getenv("AGENTRY_AUTHPROXY_EXEC")) != ""
}

// startChild spawns the user's command and returns a Childproc handle.
// The child inherits authproxy's env, with PORT swapped to the
// upstream port so an app reading process.env.PORT lands on the right
// listener.
func startChild(cfg *Config) (*Childproc, error) {
	raw := strings.TrimSpace(os.Getenv("AGENTRY_AUTHPROXY_EXEC"))
	if raw == "" {
		return nil, errors.New("AGENTRY_AUTHPROXY_EXEC is empty")
	}
	argv := splitArgv(raw)
	if len(argv) == 0 {
		return nil, errors.New("AGENTRY_AUTHPROXY_EXEC parsed to zero args")
	}

	upstreamPort, err := resolveUpstreamPort(cfg)
	if err != nil {
		return nil, err
	}
	// Rewrite cfg.Upstream to the loopback at the new port so the
	// proxy.go reverse proxy lands on the child we just spawned.
	cfg.Upstream = "127.0.0.1:" + upstreamPort

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = childEnv(upstreamPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The child INHERITS authproxy's process group on purpose. The
	// runtime's project manager stops projects by signalling the
	// project pgid; with the child in its own pgid (the v1 design),
	// that signal only reached authproxy — `npm run dev`'s subtree
	// (node → next-server) survived as an orphan still holding the
	// upstream port, and the next project_start died with
	// EADDRINUSE. One shared pgid means one kill takes the whole
	// tree, every time.

	log.Printf("authproxy: exec: %s (upstream PORT=%s)", raw, upstreamPort)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %q: %w", argv[0], err)
	}
	return &Childproc{cmd: cmd, raw: raw}, nil
}

// resolveUpstreamPort picks the port the child app should bind to.
// Preference order: explicit AGENTRY_AUTHPROXY_UPSTREAM_PORT → PORT+1.
// We refuse a value identical to authproxy's listen port — that's a
// recipe for the proxy hammering its own listener.
func resolveUpstreamPort(cfg *Config) (string, error) {
	if v := strings.TrimSpace(os.Getenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT")); v != "" {
		if v == cfg.Port {
			return "", fmt.Errorf("AGENTRY_AUTHPROXY_UPSTREAM_PORT=%s collides with PORT", v)
		}
		if _, err := strconv.Atoi(v); err != nil {
			return "", fmt.Errorf("AGENTRY_AUTHPROXY_UPSTREAM_PORT=%q is not a number", v)
		}
		return v, nil
	}
	p, err := strconv.Atoi(cfg.Port)
	if err != nil {
		return "", fmt.Errorf("PORT=%q is not a number", cfg.Port)
	}
	if p+1 > 65535 {
		return "", fmt.Errorf("PORT=%d leaves no room for upstream", p)
	}
	return strconv.Itoa(p + 1), nil
}

// childEnv builds the env the child inherits. Copies authproxy's env
// minus PORT (which we override) and minus AGENTRY_AUTHPROXY_EXEC
// (so the child can't accidentally recursively re-spawn if it also
// loads our binary).
func childEnv(upstreamPort string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PORT=") {
			continue
		}
		if strings.HasPrefix(kv, "AGENTRY_AUTHPROXY_EXEC=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "PORT="+upstreamPort)
	return out
}

// Childproc wraps the spawned process for clean shutdown.
type Childproc struct {
	cmd *exec.Cmd
	raw string
}

// Wait blocks until the child exits and returns its exit code. -1 if
// we couldn't read one.
func (c *Childproc) Wait() int {
	err := c.cmd.Wait()
	if err == nil {
		return 0
	}
	var exErr *exec.ExitError
	if errors.As(err, &exErr) {
		return exErr.ExitCode()
	}
	log.Printf("authproxy: child wait: %v", err)
	return -1
}

// Shutdown SIGTERMs the child process directly, waits up to 15 s,
// then SIGKILLs it. The child shares OUR pgid (see startChild), so
// when the project manager initiated this shutdown via a group
// signal, the whole subtree already received SIGTERM directly —
// this path is just authproxy's own grace handling, plus the case
// where authproxy exits on its own (listener error). npm forwards
// SIGTERM to its script and `next dev` tears down next-server on it,
// so a single PID-targeted signal walks the tree in practice; the
// group-level SIGKILL from the runtime is the backstop.
func (c *Childproc) Shutdown(ctx context.Context) {
	if c.cmd.Process == nil {
		return
	}
	pid := c.cmd.Process.Pid
	log.Printf("authproxy: SIGTERM child pid=%d", pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		_, _ = c.cmd.Process.Wait()
		close(done)
	}()

	deadline := 15 * time.Second
	select {
	case <-done:
		return
	case <-time.After(deadline):
		log.Printf("authproxy: child didn't exit in %s; SIGKILL", deadline)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runExecMode is the lifecycle loop for child + listener. It returns
// the exit code the parent should propagate; the caller's main()
// turns that into os.Exit().
//
// The flow:
//
//   1. Spawn child with shifted PORT.
//   2. Build the listener (the rest of authproxy's normal handlers).
//   3. Race child-exit vs signal vs listener-error.
//   4. On any of those, shut the others down and propagate the
//      first-seen exit cause.
func runExecMode(cfg *Config, listen func() error) int {
	child, err := startChild(cfg)
	if err != nil {
		log.Printf("authproxy: failed to start child: %v", err)
		return 1
	}

	childExit := make(chan int, 1)
	go func() { childExit <- child.Wait() }()

	listenErr := make(chan error, 1)
	go func() { listenErr <- listen() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case code := <-childExit:
		log.Printf("authproxy: child exited with code %d", code)
		return code
	case sig := <-sigCh:
		log.Printf("authproxy: received %s; forwarding to child", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
		defer cancel()
		child.Shutdown(shutdownCtx)
		return 0
	case err := <-listenErr:
		log.Printf("authproxy: listener exited: %v; tearing down child", err)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
		defer cancel()
		child.Shutdown(shutdownCtx)
		return 1
	}
}

// splitArgv splits an AGENTRY_AUTHPROXY_EXEC value into argv. Honors
// double-quoted segments so users can pass `--flag="value with
// spaces"` without resorting to `sh -c`. Single quotes intentionally
// not honored — they're rare in JSON-stamped env vars and supporting
// both forms adds parser complexity.
func splitArgv(s string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
