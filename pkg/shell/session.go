// Package shell provides persistent PTY-based bash sessions for command execution.
//
// Each session maintains a long-running bash process with a pseudo-terminal,
// allowing stateful command execution (cd, env vars persist across commands).
// Commands are delimited by unique markers to reliably detect completion.
package shell

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	// DefaultTimeout is the fallback command execution timeout in seconds
	// when the caller doesn't pass an explicit value. Sized for "the
	// model forgot to set a timeout on a pip install" — long enough that
	// medium installs finish, short enough that a genuinely-hung command
	// returns control inside one MCP call. The MCP tool schema teaches
	// the model to set a per-command timeout (300+ for installs, 60 for
	// quick checks); this default just keeps the failure mode benign.
	DefaultTimeout = 120.0

	// SessionIdleExpiry is how long a session can be idle before cleanup.
	SessionIdleExpiry = 30 * time.Minute

	readBufSize = 4096

	// MaxConcurrent limits parallel command execution across sessions.
	MaxConcurrent = 4
)

// Session represents a persistent bash shell backed by a PTY.
type Session struct {
	mu       sync.Mutex
	id       string
	ptmx     *os.File
	cmd      *exec.Cmd
	lastUsed time.Time
	output   chan []byte
	done     chan struct{}
}

// Manager tracks all active shell sessions with concurrency control.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	sem      chan struct{} // concurrency semaphore
}

// NewManager creates a session manager and starts the cleanup goroutine.
func NewManager() *Manager {
	concurrency := MaxConcurrent
	if env := os.Getenv("SANDBOX_CONCURRENCY"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v > 0 {
			concurrency = v
		}
	}
	m := &Manager{
		sessions: make(map[string]*Session),
		sem:      make(chan struct{}, concurrency),
	}
	go m.cleanupLoop()
	return m
}

// GetOrCreate returns an existing session or creates a new one.
func (m *Manager) GetOrCreate(id string) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	s, err := newSession(id)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		s.Close()
		return existing, nil
	}
	m.sessions[id] = s
	m.mu.Unlock()
	return s, nil
}

// Execute acquires a concurrency slot, then runs the command in the given session.
func (m *Manager) Execute(sessionID, command, execDir string, timeout float64) (string, int, string) {
	// Acquire semaphore slot.
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	s, err := m.GetOrCreate(sessionID)
	if err != nil {
		return fmt.Sprintf("session error: %v", err), -1, "terminated"
	}
	return s.Execute(command, execDir, timeout)
}

// Remove closes and removes a session.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.Close()
	}
}

// CloseAll closes all sessions.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}

func newSession(id string) (*Session, error) {
	shellPath := "/bin/bash"
	if _, err := os.Stat(shellPath); err != nil {
		shellPath = "/bin/sh"
	}

	// --norc --noprofile to skip USER rc files (no ~/.bashrc surprises),
	// then we EXPLICITLY source operator-staged /etc/profile.d/*.sh below.
	// That's what wires up /var/run/agentry/<service>/<KEY> → TRINO_URL etc.
	// for any session command_run starts.
	cmd := exec.Command(shellPath, "--norc", "--noprofile")
	cmd.Env = append(os.Environ(), "TERM=dumb", "PS1=", "PS2=")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	s := &Session{
		id:       id,
		ptmx:     ptmx,
		cmd:      cmd,
		lastUsed: time.Now(),
		output:   make(chan []byte, 256),
		done:     make(chan struct{}),
	}

	go s.readLoop()

	// Quiet the shell, then pull in operator-staged env (creds, paths,
	// etc.). Any noise from the profile scripts is drained below.
	s.writeRaw("stty -echo\nexport PS1=''\nexport PS2=''\n")
	s.writeRaw(profileSourceSnippet)
	s.drainFor(300 * time.Millisecond)

	return s, nil
}

// profileSourceSnippet sources every readable /etc/profile.d/*.sh in
// the session's own context. Mirrors what a login shell would do.
// Errors are swallowed: a single broken profile script shouldn't
// poison the session, and the file glob safely no-ops if the dir is
// empty.
const profileSourceSnippet = "for _f in /etc/profile.d/*.sh; do [ -r \"$_f\" ] && . \"$_f\" >/dev/null 2>&1; done; unset _f\n"

func (s *Session) readLoop() {
	defer close(s.done)
	buf := make([]byte, readBufSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.output <- chunk:
			default:
				select {
				case <-s.output:
				default:
				}
				s.output <- chunk
			}
		}
		if err != nil {
			if err != io.EOF {
				// PTY closed or error — session is done.
			}
			return
		}
	}
}

func (s *Session) writeRaw(data string) {
	_, _ = s.ptmx.Write([]byte(data))
}

func (s *Session) drainFor(dur time.Duration) {
	deadline := time.After(dur)
	for {
		select {
		case <-s.output:
		case <-deadline:
			return
		}
	}
}

// Execute runs a command in the session and blocks until completion or timeout.
func (s *Session) Execute(command, execDir string, timeout float64) (string, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()

	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	marker := fmt.Sprintf("__SANDBOX_END_%d_%d__", time.Now().UnixNano(), rand.Intn(999999))

	var cmdStr string
	if execDir != "" {
		cmdStr = fmt.Sprintf("cd %s 2>/dev/null\n%s\necho \"%s $?\"\n", shellQuote(execDir), command, marker)
	} else {
		cmdStr = fmt.Sprintf("%s\necho \"%s $?\"\n", command, marker)
	}

	// Drain any stale output left by a previous command. 10 ms is the
	// shortest interval that reliably catches a slow final newline from
	// bash's prompt; longer values just add dead time to every call.
	s.drainFor(10 * time.Millisecond)

	if _, err := s.ptmx.Write([]byte(cmdStr)); err != nil {
		return fmt.Sprintf("failed to write to pty: %v", err), -1, "terminated"
	}

	deadline := time.After(time.Duration(timeout * float64(time.Second)))
	var collected bytes.Buffer

	for {
		select {
		case chunk := <-s.output:
			collected.Write(chunk)
			if idx := strings.Index(collected.String(), marker); idx >= 0 {
				full := collected.String()
				before := full[:idx]
				after := full[idx+len(marker):]
				exitCode := parseExitCode(after)
				output := strings.TrimRight(before, "\r\n")
				return output, exitCode, statusFromCode(exitCode)
			}
		case <-deadline:
			return collected.String(), -1, "hard_timeout"
		case <-s.done:
			return collected.String(), -1, "terminated"
		}
	}
}

// Close terminates the session.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}

func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		var expired []string
		for id, s := range m.sessions {
			s.mu.Lock()
			if time.Since(s.lastUsed) > SessionIdleExpiry {
				expired = append(expired, id)
			}
			s.mu.Unlock()
		}
		for _, id := range expired {
			if s, ok := m.sessions[id]; ok {
				delete(m.sessions, id)
				go s.Close()
			}
		}
		m.mu.Unlock()
	}
}

func parseExitCode(s string) int {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) > 0 {
		if code, err := strconv.Atoi(fields[0]); err == nil {
			return code
		}
	}
	return -1
}

func statusFromCode(code int) string {
	if code >= 0 {
		return "completed"
	}
	return "terminated"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
