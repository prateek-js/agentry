package jupyter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Tunables for the kernel-host layer.
const (
	// MaxKernels caps live kernels. Each Python kernel is ~30 MiB
	// resident; a sandbox VM with 4 GiB safely runs ~30 concurrent.
	MaxKernels = 16

	// KernelReadyTimeout is the cold-start budget for a new kernel.
	KernelReadyTimeout = 10 * time.Second

	// KernelShutdownGrace is how long Shutdown waits before killing
	// the process group.
	KernelShutdownGrace = 3 * time.Second

	// KernelIdleExpiry kills a kernel that hasn't been Execute'd
	// against in this long. Holds state across multi-turn agent
	// loops without leaking on abandoned contexts.
	KernelIdleExpiry = 30 * time.Minute

	managerGCInterval = 5 * time.Minute
)

// ErrKernelCapacity is returned by Spawn when MaxKernels is reached.
var ErrKernelCapacity = errors.New("jupyter: kernel cap reached")

// ErrKernelNotFound is the lookup miss; HTTP layers map to 404.
var ErrKernelNotFound = errors.New("jupyter: kernel not found")

// Manager owns the lifecycle of every running kernel. Safe for
// concurrent use.
//
// The Manager holds a long-lived context that backs every kernel's
// process and ZMQ sockets. Callers pass their (often short-lived)
// request context to Spawn, which is only used for the cold-start
// readiness wait — the kernel keeps running after it returns.
type Manager struct {
	mu      sync.RWMutex
	kernels map[string]*kernelEntry
	sem     chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	stop chan struct{}
	once sync.Once
}

type kernelEntry struct {
	k        *Kernel
	lastUsed atomic.Int64 // nanoseconds since Unix epoch
}

// NewManager constructs a manager and starts its idle-GC loop.
func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		kernels: make(map[string]*kernelEntry),
		sem:     make(chan struct{}, MaxKernels),
		ctx:     ctx,
		cancel:  cancel,
		stop:    make(chan struct{}),
	}
	go m.gcLoop()
	return m
}

// Spawn starts a new kernel for language and registers it as id. If
// id is empty, a random one is generated. Returns the running Kernel.
//
// readyCtx is used only for the cold-start readiness wait. The kernel
// process and its ZMQ sockets are bound to the Manager's own context
// so they survive after the originating HTTP request returns.
func (m *Manager) Spawn(readyCtx context.Context, id, language string) (*Kernel, error) {
	if id == "" {
		id = mustRandomID()
	}
	// Quick presence check.
	m.mu.RLock()
	if _, exists := m.kernels[id]; exists {
		m.mu.RUnlock()
		return nil, fmt.Errorf("kernel id %q already in use", id)
	}
	m.mu.RUnlock()

	// Reserve a slot.
	select {
	case m.sem <- struct{}{}:
	default:
		return nil, ErrKernelCapacity
	}

	k, err := StartKernel(m.ctx, readyCtx, id, language, KernelReadyTimeout)
	if err != nil {
		<-m.sem
		return nil, err
	}

	entry := &kernelEntry{k: k}
	entry.lastUsed.Store(time.Now().UnixNano())

	m.mu.Lock()
	if _, exists := m.kernels[id]; exists {
		m.mu.Unlock()
		// Race lost — somebody else registered concurrently. Tear ours down.
		_ = k.Shutdown(KernelShutdownGrace)
		<-m.sem
		return nil, fmt.Errorf("kernel id %q already in use", id)
	}
	m.kernels[id] = entry
	m.mu.Unlock()

	// Release the semaphore when the kernel finishes (and delete the
	// map entry so a re-Spawn with the same id can succeed).
	go func() {
		<-k.done
		m.mu.Lock()
		if cur, ok := m.kernels[id]; ok && cur == entry {
			delete(m.kernels, id)
		}
		m.mu.Unlock()
		<-m.sem
	}()

	return k, nil
}

// Get returns the kernel for id, or ErrKernelNotFound.
func (m *Manager) Get(id string) (*Kernel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.kernels[id]
	if !ok {
		return nil, ErrKernelNotFound
	}
	return e.k, nil
}

// Touch updates the last-used timestamp on a kernel — call from the
// HTTP execute handler so the idle reaper doesn't drop active sessions.
func (m *Manager) Touch(id string) {
	m.mu.RLock()
	e, ok := m.kernels[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	e.lastUsed.Store(time.Now().UnixNano())
}

// Shutdown stops one kernel by id. Idempotent.
func (m *Manager) Shutdown(id string) error {
	m.mu.RLock()
	e, ok := m.kernels[id]
	m.mu.RUnlock()
	if !ok {
		return ErrKernelNotFound
	}
	return e.k.Shutdown(KernelShutdownGrace)
}

// List returns an info snapshot for every live kernel.
func (m *Manager) List() []KernelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]KernelInfo, 0, len(m.kernels))
	for id, e := range m.kernels {
		out = append(out, KernelInfo{
			ID:       id,
			Language: e.k.language,
			Closed:   e.k.IsClosed(),
			IdleNS:   time.Now().UnixNano() - e.lastUsed.Load(),
		})
	}
	return out
}

// KernelInfo is the summary surfaced by /v1/code/contexts list.
type KernelInfo struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Closed   bool   `json:"closed"`
	IdleNS   int64  `json:"idle_ns"`
}

// Close terminates every kernel and stops the GC loop. Idempotent.
func (m *Manager) Close() {
	m.once.Do(func() {
		close(m.stop)
		m.cancel()
	})
	m.mu.Lock()
	toKill := make([]*Kernel, 0, len(m.kernels))
	for _, e := range m.kernels {
		toKill = append(toKill, e.k)
	}
	m.kernels = make(map[string]*kernelEntry)
	m.mu.Unlock()
	for _, k := range toKill {
		_ = k.Shutdown(KernelShutdownGrace)
	}
}

func (m *Manager) gcLoop() {
	t := time.NewTicker(managerGCInterval)
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

// gcOnce shuts down kernels whose last Execute is older than
// KernelIdleExpiry. Parameterized on now so tests stay deterministic.
func (m *Manager) gcOnce(now time.Time) int {
	m.mu.RLock()
	stale := make([]*Kernel, 0)
	for _, e := range m.kernels {
		last := time.Unix(0, e.lastUsed.Load())
		if now.Sub(last) >= KernelIdleExpiry {
			stale = append(stale, e.k)
		}
	}
	m.mu.RUnlock()
	for _, k := range stale {
		_ = k.Shutdown(KernelShutdownGrace)
	}
	return len(stale)
}
