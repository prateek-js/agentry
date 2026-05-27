package provisioner

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// reaperGracePeriod is the SIGTERM grace period passed to Pod deletion when
// the reaper culls an expired sandbox. Short — the sandbox is already past
// its declared lifetime, but we still let containers drain.
const reaperGracePeriod = int64(10)

// reaperConcurrency caps parallel deletion fan-out per cycle. A single tick
// could otherwise spawn N goroutines and hammer the API server.
const reaperConcurrency = 8

// Reaper deletes sandboxes whose ad-sandbox.io/expires-at annotation is in
// the past. It is started by Provisioner.RunReaper and runs until its
// context is canceled.
type Reaper struct {
	backend   Backend
	namespace string
	interval  time.Duration

	// now is the clock function; tests replace it for determinism.
	now func() time.Time
}

// NewReaper builds a reaper. interval <= 0 disables the loop (Run returns
// immediately).
func NewReaper(backend Backend, namespace string, interval time.Duration) *Reaper {
	return &Reaper{
		backend:   backend,
		namespace: namespace,
		interval:  interval,
		now:       time.Now,
	}
}

// Run loops until ctx is canceled, reaping every interval. A first sweep
// happens immediately so freshly-started provisioners catch any sandboxes
// whose deadlines fell during the outage.
//
// Returns ctx.Err() on shutdown; never logs-and-exits silently.
func (r *Reaper) Run(ctx context.Context) error {
	if r.interval <= 0 {
		log.Printf("reaper disabled (interval=%s)", r.interval)
		return nil
	}
	log.Printf("reaper started: namespace=%s interval=%s", r.namespace, r.interval)

	// Tick once immediately so we don't wait `interval` before the first
	// sweep on startup.
	r.sweep(ctx)

	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("reaper stopped: %v", ctx.Err())
			return ctx.Err()
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

// sweep lists sandboxes once and concurrently deletes any that are expired.
// Errors are logged but do not stop the loop — transient API errors are
// expected during cluster upgrades or network blips.
func (r *Reaper) sweep(ctx context.Context) {
	sandboxes, err := r.backend.ListSandboxes(ctx, r.namespace, nil)
	if err != nil {
		log.Printf("reaper list failed: %v", err)
		return
	}

	now := r.now()
	expired := make([]string, 0, len(sandboxes))
	for _, s := range sandboxes {
		t, perr := parseExpiresAt(s.ExpiresAt)
		if perr != nil {
			log.Printf("reaper: skipping %s with bad expires-at: %v", s.SandboxID, perr)
			continue
		}
		if isExpired(t, now) {
			expired = append(expired, s.SandboxID)
		}
	}
	if len(expired) == 0 {
		return
	}

	r.reapAll(ctx, expired)
}

// reapAll deletes the given sandbox IDs in parallel, bounded by
// reaperConcurrency. Each deletion best-efforts both Pod and Service so a
// half-deleted state is cleaned up on the next pass.
func (r *Reaper) reapAll(ctx context.Context, ids []string) {
	sem := make(chan struct{}, reaperConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.reapOne(ctx, sid)
		}(id)
	}
	wg.Wait()
}

func (r *Reaper) reapOne(ctx context.Context, id string) {
	podName := "sandbox-" + id
	svcName := podName + "-svc"

	if err := r.backend.DeletePod(ctx, r.namespace, podName, reaperGracePeriod); err != nil &&
		!strings.Contains(err.Error(), "not found") {
		log.Printf("reaper: delete pod %s: %v", podName, err)
	}
	if err := r.backend.DeleteService(ctx, r.namespace, svcName); err != nil &&
		!strings.Contains(err.Error(), "not found") {
		log.Printf("reaper: delete svc %s: %v", svcName, err)
	}
	log.Printf("reaper: deleted expired sandbox %s", id)
}
