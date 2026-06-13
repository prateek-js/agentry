package handlers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The port cache fronts the per-poll `ss` fork that project_list drives.
// These pin: a hit within TTL skips the fetch, an expiry re-fetches, and
// concurrent lookups for different pgids don't serialize on the lock.

func resetPortCache() {
	portCacheMu.Lock()
	portCache = map[int]portCacheEntry{}
	portCacheMu.Unlock()
}

func TestCachedPorts_HitSkipsFetch(t *testing.T) {
	resetPortCache()
	defer resetPortCache()

	var calls int32
	fetch := func(pgid int) []int {
		atomic.AddInt32(&calls, 1)
		return []int{3000, 5432}
	}

	a := cachedPorts(42, fetch)
	b := cachedPorts(42, fetch)
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("fetch ran %d times; want 1 (second call should hit cache)", calls)
	}
	if len(a) != 2 || len(b) != 2 || b[0] != 3000 {
		t.Errorf("cached value wrong: a=%v b=%v", a, b)
	}
}

func TestCachedPorts_ExpiryRefetches(t *testing.T) {
	resetPortCache()
	defer resetPortCache()

	// Force every call to miss.
	orig := portCacheTTL
	portCacheTTL = 0
	defer func() { portCacheTTL = orig }()

	var calls int32
	fetch := func(pgid int) []int {
		atomic.AddInt32(&calls, 1)
		return []int{8080}
	}
	cachedPorts(7, fetch)
	cachedPorts(7, fetch)
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("with TTL=0 both calls should fetch; got %d", calls)
	}
}

func TestCachedPorts_ZeroPGIDIsNil(t *testing.T) {
	resetPortCache()
	called := false
	got := cachedPorts(0, func(int) []int { called = true; return []int{1} })
	if got != nil {
		t.Errorf("pgid<=0 should return nil; got %v", got)
	}
	if called {
		t.Error("pgid<=0 should not invoke fetch")
	}
}

func TestCachedPorts_ConcurrentDifferentPGIDs(t *testing.T) {
	resetPortCache()
	defer resetPortCache()

	// Each fetch blocks briefly; if the cache serialized all callers on
	// the lock during fetch, 20 goroutines × 20ms would take >400ms.
	// Released-lock-during-fetch keeps it near one fetch duration.
	fetch := func(pgid int) []int {
		time.Sleep(20 * time.Millisecond)
		return []int{pgid}
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(pgid int) {
			defer wg.Done()
			cachedPorts(pgid, fetch)
		}(i + 1)
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("concurrent lookups serialized (%v) — fetch must run outside the lock", elapsed)
	}
}
