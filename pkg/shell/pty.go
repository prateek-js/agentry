package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Tunables for the interactive PTY layer.
const (
	// PTYReplaySize is the per-PTY ring buffer used to replay recent
	// output to a freshly-attached client. 64 KiB ≈ several screens of
	// terminal output; enough to feel "stitched" after a reconnect.
	PTYReplaySize = 64 << 10

	// PTYMaxConcurrent caps the number of live PTYs. Each one owns a
	// running bash process so the ceiling protects against accidental
	// fork bombs from runaway clients.
	PTYMaxConcurrent = 32

	// PTYReadBuf is the per-iteration read size from /dev/ptmx.
	PTYReadBuf = 4096

	// PTYIdleExpiry kills a PTY that has had no attached client and no
	// recent output for this long. Holds the process tree alive across
	// brief disconnects but doesn't leak forever.
	PTYIdleExpiry = 30 * time.Minute

	ptyCleanupInterval = 5 * time.Minute
)

// ErrPTYBusy is returned by AttachStream when another client is already
// attached. HTTP layers map this to 409.
var ErrPTYBusy = errors.New("pty: another client is attached")

// ErrPTYClosed is returned by AttachStream after the PTY's bash has
// exited (and the manager has marked it done). HTTP layers map this to
// 410.
var ErrPTYClosed = errors.New("pty: closed")

// PTYWriter is the subset of io.Writer that AttachStream uses to push
// output to the client. The WebSocket handler implements it.
type PTYWriter interface {
	// WriteOutput is called with a chunk of bytes that should reach the
	// client as a single stdout frame. Implementations should return
	// promptly; AttachStream serializes calls.
	WriteOutput(b []byte) error
	// WriteExit is called once when the PTY's process exits.
	WriteExit(code int) error
}

// PTY is a long-lived interactive shell PTY. Unlike Session (markered
// blocking exec), it streams raw bytes to a single attached client.
type PTY struct {
	id     string
	cmd    *exec.Cmd
	ptmx   *os.File
	replay *Ring
	notify chan struct{} // buffered=1; pinged on every Write

	mu       sync.Mutex
	attached bool         // true while a client owns this PTY
	closed   bool         // true once Close was called or bash exited
	exitCode atomic.Int32 // -1 until set
	lastUsed atomic.Int64 // unix-nano; updated by readLoop & WriteStdin

	done chan struct{} // closed when the bash process exits
}

// newPTY spawns a fresh bash under a pty with the requested window size.
// rows/cols default to 24/80 when zero.
func newPTY(id string, rows, cols uint16) (*PTY, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	shellPath := "/bin/bash"
	if _, err := os.Stat(shellPath); err != nil {
		shellPath = "/bin/sh"
	}
	// -l makes bash treat itself as a login shell, so /etc/profile +
	// /etc/profile.d/*.sh are sourced — that's how operator-staged
	// creds (TRINO_URL, etc.) reach this PTY. -i keeps it interactive
	// for prompt/line-editing behaviour.
	cmd := exec.Command(shellPath, "-l", "-i")
	// A real interactive TERM unlocks colors / line editing. We export
	// HOME / USER for shells that bail otherwise.
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	p := &PTY{
		id:     id,
		cmd:    cmd,
		ptmx:   ptmx,
		replay: NewRing(PTYReplaySize),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	p.exitCode.Store(-1)
	p.lastUsed.Store(time.Now().UnixNano())
	go p.readLoop()
	go p.waitLoop()
	return p, nil
}

func (p *PTY) readLoop() {
	buf := make([]byte, PTYReadBuf)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			_, _ = p.replay.Write(buf[:n])
			p.lastUsed.Store(time.Now().UnixNano())
			// Non-blocking notify: if a notification is already pending
			// the consumer hasn't run yet, so we don't need another.
			select {
			case p.notify <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *PTY) waitLoop() {
	err := p.cmd.Wait()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	p.exitCode.Store(int32(code))
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	// Drain ptmx briefly so any final output gets into the replay buffer
	// before we close it.
	_ = p.ptmx.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, _ = io.Copy(p.replay, p.ptmx)
	_ = p.ptmx.Close()
	close(p.done)
	// Final notify so AttachStream wakes and can send the exit frame.
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// AttachStream takes over as the single output sink for this PTY,
// sends the replay snapshot, then tails the ring buffer until either
// ctx is cancelled or the bash process exits.
//
// Returns:
//
//	ErrPTYBusy   another client is currently attached
//	ErrPTYClosed bash has already exited
//	nil          ctx was cancelled cleanly
//	other        the client writer returned an error
func (p *PTY) AttachStream(ctx context.Context, w PTYWriter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPTYClosed
	}
	if p.attached {
		p.mu.Unlock()
		return ErrPTYBusy
	}
	p.attached = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.attached = false
		p.mu.Unlock()
	}()

	// Initial replay snapshot — everything we've kept in the ring so far.
	snap, cursor, _ := p.replay.Snapshot()
	if len(snap) > 0 {
		if err := w.WriteOutput(snap); err != nil {
			return err
		}
	}

	// Tail loop: wait for new output (or exit), then drain the ring
	// using the cursor. We don't bound iterations — the only exit
	// conditions are ctx cancellation, process exit, or a write error.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.done:
			// Final drain of anything written after the last notify.
			data, newCur, _ := p.replay.Read(cursor)
			cursor = newCur
			if len(data) > 0 {
				_ = w.WriteOutput(data)
			}
			return w.WriteExit(int(p.exitCode.Load()))
		case <-p.notify:
			data, newCur, _ := p.replay.Read(cursor)
			cursor = newCur
			if len(data) == 0 {
				continue
			}
			if err := w.WriteOutput(data); err != nil {
				return err
			}
		}
	}
}

// WriteStdin pipes b into the pty.
func (p *PTY) WriteStdin(b []byte) error {
	p.lastUsed.Store(time.Now().UnixNano())
	_, err := p.ptmx.Write(b)
	return err
}

// Resize updates the pty window size. Background readers don't need a
// barrier here — Setsize is synchronous and the next read picks up the
// new geometry.
func (p *PTY) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return errors.New("rows and cols must be > 0")
	}
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close terminates bash and releases the pty. Idempotent.
func (p *PTY) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.ptmx.Close()
}

// IsAttached reports whether a client is currently bound to this PTY.
func (p *PTY) IsAttached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attached
}

// IsClosed reports whether the PTY's bash has exited.
func (p *PTY) IsClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// PTYManager keeps a map of id → *PTY and bounds concurrency.
type PTYManager struct {
	mu   sync.Mutex
	ptys map[string]*PTY
	sem  chan struct{}

	stop chan struct{}
	once sync.Once
}

// NewPTYManager constructs a manager and starts a GC loop that reaps
// idle PTYs whose last read/write is older than PTYIdleExpiry.
func NewPTYManager() *PTYManager {
	m := &PTYManager{
		ptys: make(map[string]*PTY),
		sem:  make(chan struct{}, PTYMaxConcurrent),
		stop: make(chan struct{}),
	}
	go m.cleanupLoop()
	return m
}

// GetOrCreate returns the existing PTY for `id`, or spawns a new bash
// pty with the given window size. Errors when the concurrency cap is
// reached.
func (m *PTYManager) GetOrCreate(id string, rows, cols uint16) (*PTY, error) {
	m.mu.Lock()
	if p, ok := m.ptys[id]; ok && !p.IsClosed() {
		m.mu.Unlock()
		return p, nil
	}
	m.mu.Unlock()

	select {
	case m.sem <- struct{}{}:
	default:
		return nil, fmt.Errorf("pty cap (%d) exhausted", PTYMaxConcurrent)
	}

	p, err := newPTY(id, rows, cols)
	if err != nil {
		<-m.sem
		return nil, err
	}

	m.mu.Lock()
	// Lost the race against a concurrent GetOrCreate: keep the one
	// that landed first and close ours.
	if existing, ok := m.ptys[id]; ok && !existing.IsClosed() {
		m.mu.Unlock()
		p.Close()
		<-m.sem
		return existing, nil
	}
	m.ptys[id] = p
	m.mu.Unlock()

	// Schedule a sem release whenever the pty really finishes (process
	// exit OR explicit close). cleanupLoop also deletes the entry from
	// the map; this goroutine just releases the semaphore.
	go func() {
		<-p.done
		<-m.sem
		m.mu.Lock()
		// Defensive: only delete if it's still our pointer.
		if cur, ok := m.ptys[id]; ok && cur == p {
			delete(m.ptys, id)
		}
		m.mu.Unlock()
	}()
	return p, nil
}

// Get returns an existing pty, or nil/false if absent.
func (m *PTYManager) Get(id string) (*PTY, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.ptys[id]
	return p, ok
}

// Close terminates a pty by id.
func (m *PTYManager) Close(id string) bool {
	m.mu.Lock()
	p, ok := m.ptys[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	p.Close()
	return true
}

// CloseAll shuts down every tracked PTY. Idempotent.
func (m *PTYManager) CloseAll() {
	m.once.Do(func() {
		close(m.stop)
	})
	m.mu.Lock()
	ptys := make([]*PTY, 0, len(m.ptys))
	for _, p := range m.ptys {
		ptys = append(ptys, p)
	}
	m.ptys = make(map[string]*PTY)
	m.mu.Unlock()
	for _, p := range ptys {
		p.Close()
	}
}

// List returns the ids and attach state of every live PTY. Useful for a
// /v1/shell/pty (no id) listing endpoint if we add one.
func (m *PTYManager) List() []PTYInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PTYInfo, 0, len(m.ptys))
	for id, p := range m.ptys {
		out = append(out, PTYInfo{
			ID:       id,
			Attached: p.IsAttached(),
			Closed:   p.IsClosed(),
		})
	}
	return out
}

// PTYInfo is the summary shape returned by List.
type PTYInfo struct {
	ID       string `json:"id"`
	Attached bool   `json:"attached"`
	Closed   bool   `json:"closed"`
}

func (m *PTYManager) cleanupLoop() {
	t := time.NewTicker(ptyCleanupInterval)
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

func (m *PTYManager) gcOnce(now time.Time) int {
	m.mu.Lock()
	var expired []*PTY
	for _, p := range m.ptys {
		if p.IsAttached() {
			continue
		}
		last := time.Unix(0, p.lastUsed.Load())
		if now.Sub(last) >= PTYIdleExpiry {
			expired = append(expired, p)
		}
	}
	m.mu.Unlock()
	for _, p := range expired {
		p.Close()
	}
	return len(expired)
}
