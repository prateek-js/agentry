package handlers

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ide.go — lazy supervisor for the in-browser editor (code-server).
//
// code-server runs on loopback only; it's never exposed directly. The
// browser reaches it through the same chain every app port uses:
//
//	dashboard → control plane → bridge → provisioner → runtime
//	  → /v1/proxy/8088/…  (AppProxyHandler) → 127.0.0.1:8088
//
// so there's no new transport. We start it on demand (most sandboxes
// never open the editor) and keep it to editor + terminal — no telemetry,
// no update checks, no workspace-trust prompt.

const (
	ideAddr = "127.0.0.1:8088"
	idePort = 8088
	ideData = "/tmp/agentry-ide"
)

var (
	ideMu  sync.Mutex
	ideCmd *exec.Cmd
)

// ideSettings strips code-server to the essentials on first launch.
const ideSettings = `{
  "telemetry.telemetryLevel": "off",
  "workbench.startupEditor": "none",
  "workbench.tips.enabled": false,
  "update.mode": "none",
  "security.workspace.trust.enabled": false,
  "window.menuBarVisibility": "compact"
}`

// IDEStartHandler launches code-server (idempotent) and returns its port.
// POST /v1/ide/start. The editor is then served via /v1/proxy/8088/.
func IDEStartHandler(w http.ResponseWriter, _ *http.Request) {
	if err := ensureIDE(); err != nil {
		Error(w, http.StatusInternalServerError, "start editor: "+err.Error())
		return
	}
	Success(w, "editor running", map[string]any{"port": idePort})
}

func ensureIDE() error {
	ideMu.Lock()
	defer ideMu.Unlock()
	if ideAlive() {
		return nil
	}
	bin, err := exec.LookPath("code-server")
	if err != nil {
		return fmt.Errorf("code-server not installed in this runtime")
	}
	if err := os.MkdirAll(filepath.Join(ideData, "User"), 0o700); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(ideData, "User", "settings.json"), []byte(ideSettings), 0o600)

	cmd := exec.Command(bin,
		"--auth", "none", // loopback only; the proxy chain is the auth boundary
		"--bind-addr", ideAddr,
		"--disable-telemetry",
		"--disable-update-check",
		"--disable-workspace-trust",
		"--user-data-dir", ideData,
		"--extensions-dir", filepath.Join(ideData, "ext"),
		"/workspace",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	ideCmd = cmd
	go func() { _ = cmd.Wait() }() // reap on exit

	// Wait until it's accepting connections (first launch unpacks).
	for i := 0; i < 150; i++ {
		if c, e := net.DialTimeout("tcp", ideAddr, 300*time.Millisecond); e == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("editor did not come up on %s", ideAddr)
}

// ideAlive reports whether code-server is up — process alive (if we
// launched it) and the port accepting.
func ideAlive() bool {
	if ideCmd != nil && ideCmd.Process != nil {
		if err := ideCmd.Process.Signal(syscall.Signal(0)); err != nil {
			return false
		}
	}
	c, err := net.DialTimeout("tcp", ideAddr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
