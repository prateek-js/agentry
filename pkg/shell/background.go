package shell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Tunables for background command lifecycle.
const (
	// BgRingSize is the per-command log buffer capacity. Old bytes are
	// evicted past this — the cursor-based Read API surfaces drops to
	// callers so they can decide whether to flag a gap to the user.
	BgRingSize = 1 << 20 // 1 MiB

	// BgMaxConcurrent caps in-flight background commands. Goroutine
	// count is bounded by this; extra Start calls block on the semaphore.
	BgMaxConcurrent = 32

	// BgRetentionAfterFinish keeps finished command state around so
	// status/logs remain queryable. Set to 0 to disable GC.
	BgRetentionAfterFinish = 30 * time.Minute

	// BgInterruptGrace is how long we wait between SIGTERM and SIGKILL
	// during Interrupt.
	BgInterruptGrace = 5 * time.Second

	bgGCInterval = 5 * time.Minute
)

// Background statuses (mirrors models.* shape for HTTP serialization).
const (
	BgStatusRunning     = "running"
	BgStatusCompleted   = "completed"
	BgStatusFailed      = "failed"
	BgStatusInterrupted = "interrupted"
)

// BgCommand is one supervised background command. Field access is
// protected by mu — except for `id`, `command`, `startedAt`, and `logs`
// which are immutable / internally locked.
type BgCommand struct {
	id      string
	command string
	execDir string

	logs *Ring

	startedAt time.Time

	mu         sync.RWMutex
	status     string
	pid        int
	exitCode   int
	finishedAt time.Time
	err        string
	cmd        *exec.Cmd
	cancel     context.CancelFunc
}

// BgStatus is the snapshot of a command's state surfaced over HTTP.
type BgStatus struct {
	ID           string `json:"id"`
	Command      string `json:"command"`
	ExecDir      string `json:"exec_dir,omitempty"`
	Status       string `json:"status"`
	PID          int    `json:"pid,omitempty"`
	ExitCode     int    `json:"exit_code"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	Error        string `json:"error,omitempty"`
	BytesOut     int64  `json:"bytes_out"`
	BytesDropped int64  `json:"bytes_dropped,omitempty"`
}

// Status returns a snapshot under a read lock.
func (b *BgCommand) Status() BgStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := BgStatus{
		ID:           b.id,
		Command:      b.command,
		ExecDir:      b.execDir,
		Status:       b.status,
		PID:          b.pid,
		ExitCode:     b.exitCode,
		StartedAt:    b.startedAt.UTC().Format(time.RFC3339Nano),
		Error:        b.err,
		BytesOut:     b.logs.Written(),
		BytesDropped: b.logs.Dropped(),
	}
	if !b.finishedAt.IsZero() {
		out.FinishedAt = b.finishedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// BackgroundManager owns the lifecycle of every backgrounded command.
// Safe for concurrent use.
type BackgroundManager struct {
	mu       sync.Mutex
	commands map[string]*BgCommand
	sem      chan struct{}

	// closed when Shutdown is invoked so the GC loop exits cleanly.
	stop chan struct{}
	once sync.Once

	// idCounter is a fallback when crypto/rand fails (extremely rare).
	idCounter atomic.Uint64
}

// NewBackgroundManager constructs a manager and launches the GC loop.
func NewBackgroundManager() *BackgroundManager {
	m := &BackgroundManager{
		commands: make(map[string]*BgCommand),
		sem:      make(chan struct{}, BgMaxConcurrent),
		stop:     make(chan struct{}),
	}
	go m.gcLoop()
	return m
}

// Shutdown stops the GC loop and interrupts every still-running command.
// Idempotent.
func (m *BackgroundManager) Shutdown() {
	m.once.Do(func() {
		close(m.stop)
		m.mu.Lock()
		toKill := make([]*BgCommand, 0, len(m.commands))
		for _, c := range m.commands {
			toKill = append(toKill, c)
		}
		m.mu.Unlock()
		for _, c := range toKill {
			_ = m.Interrupt(c.id)
		}
	})
}

// Start spawns a new background command. Returns its assigned id.
//
// command is the shell string; we run it under `/bin/sh -c "<command>"`
// so users can use pipes, redirections, etc. The process is placed in
// its own process group so Interrupt can SIGTERM the whole tree.
//
// Blocks if the concurrency cap is exhausted; pass a ctx with a
// deadline if the caller wants to bail out.
func (m *BackgroundManager) Start(ctx context.Context, command, execDir string, env []string) (string, error) {
	if command == "" {
		return "", errors.New("command is required")
	}

	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	id := m.newID()
	cmdCtx, cancel := context.WithCancel(context.Background())
	// Prepend the profile.d source loop so background commands see
	// /var/run/xdp/<svc>/<KEY> as env vars (TRINO_URL, etc.) — same
	// posture as command_run's session shells.
	wrapped := "for _f in /etc/profile.d/*.sh; do [ -r \"$_f\" ] && . \"$_f\" >/dev/null 2>&1; done; unset _f\n" + command
	cmd := exec.CommandContext(cmdCtx, "/bin/bash", "-c", wrapped)
	cmd.Dir = execDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	bc := &BgCommand{
		id:        id,
		command:   command,
		execDir:   execDir,
		logs:      NewRing(BgRingSize),
		startedAt: time.Now(),
		status:    BgStatusRunning,
		cmd:       cmd,
		cancel:    cancel,
	}
	cmd.Stdout = bc.logs
	cmd.Stderr = bc.logs

	if err := cmd.Start(); err != nil {
		cancel()
		<-m.sem
		return "", fmt.Errorf("start: %w", err)
	}
	bc.mu.Lock()
	bc.pid = cmd.Process.Pid
	bc.mu.Unlock()

	m.mu.Lock()
	m.commands[id] = bc
	m.mu.Unlock()

	go m.waitFor(bc)
	return id, nil
}

// waitFor blocks on cmd.Wait and transitions state. Releases the
// concurrency slot on exit so a new command can take its place
// immediately even if the finished record is retained for log polling.
func (m *BackgroundManager) waitFor(bc *BgCommand) {
	defer func() {
		<-m.sem
		bc.cancel() // free any lingering ctx resources
	}()

	err := bc.cmd.Wait()
	now := time.Now()

	bc.mu.Lock()
	bc.finishedAt = now
	if err == nil {
		bc.status = BgStatusCompleted
		bc.exitCode = 0
	} else {
		if exitErr, ok := err.(*exec.ExitError); ok {
			bc.exitCode = exitErr.ExitCode()
			// Signal-induced exit shows up as status=interrupted only if we
			// actively interrupted; otherwise it's a regular failure.
			if bc.status != BgStatusInterrupted {
				bc.status = BgStatusFailed
				bc.err = exitErr.Error()
			}
		} else {
			bc.status = BgStatusFailed
			bc.exitCode = -1
			bc.err = err.Error()
		}
	}
	bc.mu.Unlock()
}

// Status returns the snapshot for id, or (zero, false) if unknown.
func (m *BackgroundManager) Status(id string) (BgStatus, bool) {
	m.mu.Lock()
	c, ok := m.commands[id]
	m.mu.Unlock()
	if !ok {
		return BgStatus{}, false
	}
	return c.Status(), true
}

// Logs returns bytes written since `cursor`, the new cursor, and how
// many bytes were dropped before this read could observe them.
// Returns (nil, 0, 0, false) for an unknown id.
func (m *BackgroundManager) Logs(id string, cursor int64) (data []byte, newCursor int64, dropped int64, ok bool) {
	m.mu.Lock()
	c, exists := m.commands[id]
	m.mu.Unlock()
	if !exists {
		return nil, 0, 0, false
	}
	data, newCursor, dropped = c.logs.Read(cursor)
	return data, newCursor, dropped, true
}

// Interrupt sends SIGTERM to the command's process group, waits up to
// BgInterruptGrace for it to exit, then SIGKILLs. Marks the command as
// "interrupted" once the signal is sent so waitFor doesn't overwrite
// the status with "failed".
func (m *BackgroundManager) Interrupt(id string) error {
	m.mu.Lock()
	c, ok := m.commands[id]
	m.mu.Unlock()
	if !ok {
		return errNotFound
	}

	c.mu.Lock()
	if c.status != BgStatusRunning {
		c.mu.Unlock()
		return nil
	}
	c.status = BgStatusInterrupted
	proc := c.cmd.Process
	c.mu.Unlock()

	if proc == nil {
		return nil
	}
	// Negative PID = signal the whole process group (Setpgid above).
	_ = syscall.Kill(-proc.Pid, syscall.SIGTERM)

	// Give it BgInterruptGrace to wind down, then SIGKILL the group.
	go func(pid int) {
		select {
		case <-time.After(BgInterruptGrace):
			c.mu.RLock()
			stillRunning := c.finishedAt.IsZero()
			c.mu.RUnlock()
			if stillRunning {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		case <-m.stop:
		}
	}(proc.Pid)
	return nil
}

// List returns every tracked command's status. Order is undefined.
func (m *BackgroundManager) List() []BgStatus {
	m.mu.Lock()
	out := make([]BgStatus, 0, len(m.commands))
	for _, c := range m.commands {
		out = append(out, c.Status())
	}
	m.mu.Unlock()
	return out
}

// Forget removes a finished command's bookkeeping immediately. Returns
// false if the command is still running (callers must Interrupt first).
func (m *BackgroundManager) Forget(id string) bool {
	m.mu.Lock()
	c, ok := m.commands[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	c.mu.RLock()
	running := c.status == BgStatusRunning
	c.mu.RUnlock()
	if running {
		m.mu.Unlock()
		return false
	}
	delete(m.commands, id)
	m.mu.Unlock()
	return true
}

// gcLoop reaps finished commands whose retention window has elapsed.
func (m *BackgroundManager) gcLoop() {
	if BgRetentionAfterFinish <= 0 {
		return
	}
	t := time.NewTicker(bgGCInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.gcOnce(time.Now())
		}
	}
}

// gcOnce is the testable single-pass GC. now is parameterized so tests
// can drive expiry deterministically.
func (m *BackgroundManager) gcOnce(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	reaped := 0
	for id, c := range m.commands {
		c.mu.RLock()
		expired := !c.finishedAt.IsZero() &&
			now.Sub(c.finishedAt) >= BgRetentionAfterFinish
		c.mu.RUnlock()
		if expired {
			delete(m.commands, id)
			reaped++
		}
	}
	return reaped
}

// newID returns a 16-hex-char identifier. Falls back to a counter on
// the extremely unlikely chance that crypto/rand fails — we don't want
// Start to error on that.
func (m *BackgroundManager) newID() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		n := m.idCounter.Add(1)
		return fmt.Sprintf("seq%016x", n)
	}
	return hex.EncodeToString(b[:])
}

var errNotFound = errors.New("background command not found")

// IsNotFound is true for the sentinel returned when an id is unknown.
// Exposed so handlers can map it to 404.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }
