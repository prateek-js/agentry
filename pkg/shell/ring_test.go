package shell

import (
	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRingWriteUnderCapacity(t *testing.T) {
	r := NewRing(16)
	n, err := r.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write returned %d, %v", n, err)
	}
	data, cur, dropped := r.Snapshot()
	if string(data) != "hello" || cur != 5 || dropped != 0 {
		t.Fatalf("snapshot = %q cur=%d dropped=%d", data, cur, dropped)
	}
}

func TestRingWraps(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("0123"))
	r.Write([]byte("4567")) // exactly fills
	r.Write([]byte("89AB")) // pushes "0123" out
	data, cur, dropped := r.Snapshot()
	if string(data) != "456789AB" {
		t.Fatalf("data = %q", data)
	}
	if cur != 12 || dropped != 4 {
		t.Fatalf("cur=%d dropped=%d", cur, dropped)
	}
}

func TestRingSingleWriteLargerThanCapacity(t *testing.T) {
	r := NewRing(4)
	r.Write([]byte("0123456789"))
	data, cur, dropped := r.Snapshot()
	if string(data) != "6789" {
		t.Fatalf("data = %q", data)
	}
	if cur != 10 || dropped != 6 {
		t.Fatalf("cur=%d dropped=%d", cur, dropped)
	}
}

func TestRingReadCursorAdvances(t *testing.T) {
	r := NewRing(64)
	r.Write([]byte("foo"))

	data, cur, dropped := r.Read(0)
	if string(data) != "foo" || cur != 3 || dropped != 0 {
		t.Fatalf("first read = %q cur=%d dropped=%d", data, cur, dropped)
	}

	r.Write([]byte("bar"))
	data, cur, dropped = r.Read(cur)
	if string(data) != "bar" || cur != 6 || dropped != 0 {
		t.Fatalf("incremental read = %q cur=%d dropped=%d", data, cur, dropped)
	}

	// No new data: empty slice, cursor unchanged.
	data, cur, dropped = r.Read(cur)
	if data != nil || cur != 6 || dropped != 0 {
		t.Fatalf("idle read = %q cur=%d dropped=%d", data, cur, dropped)
	}
}

func TestRingReadDetectsDroppedBytes(t *testing.T) {
	r := NewRing(4)
	r.Write([]byte("abcd")) // cursor=4, no drops
	r.Write([]byte("efgh")) // evicts "abcd" → dropped=4, written=8

	// Reader is still at cursor=0; we have only "efgh" in the ring.
	data, cur, dropped := r.Read(0)
	if string(data) != "efgh" {
		t.Errorf("data = %q; want efgh", data)
	}
	if cur != 8 || dropped != 4 {
		t.Errorf("cur=%d dropped=%d; want 8/4", cur, dropped)
	}

	// Reader catches up — subsequent reads have no drops.
	r.Write([]byte("i"))
	data, cur, dropped = r.Read(cur)
	if string(data) != "i" || cur != 9 || dropped != 0 {
		t.Errorf("post-catch-up: data=%q cur=%d dropped=%d", data, cur, dropped)
	}
}

func TestRingZeroLengthWrite(t *testing.T) {
	r := NewRing(8)
	n, err := r.Write(nil)
	if n != 0 || err != nil {
		t.Errorf("nil write returned %d, %v", n, err)
	}
	if r.Written() != 0 {
		t.Errorf("written = %d; want 0", r.Written())
	}
}

func TestRingWrapBoundary(t *testing.T) {
	// Exercise the two-segment copy in both Write and Read.
	r := NewRing(6)
	r.Write([]byte("AAA")) // head=0, size=3
	r.Write([]byte("BBB")) // head=0, size=6 (full)
	r.Write([]byte("CC"))  // overflow: drop 2, head=2, size=6
	// Buf layout (logical): "ABBBCC" starting from head=2
	data, _, _ := r.Snapshot()
	if string(data) != "ABBBCC" {
		t.Fatalf("snapshot = %q; want ABBBCC", data)
	}
}

func TestRingPanicsOnNonPositiveCap(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for cap<=0")
		}
	}()
	_ = NewRing(0)
}

// TestRingConcurrent stress-tests Write/Read interleaving. With -race,
// asserts there's no data race; functionally checks that the reader's
// view never goes backwards.
func TestRingConcurrent(t *testing.T) {
	r := NewRing(1024)
	const writers = 4
	const writesPerWriter = 200

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			buf := make([]byte, 64)
			for i := 0; i < writesPerWriter; i++ {
				n := rng.Intn(64) + 1
				r.Write(buf[:n])
			}
		}(int64(w))
	}

	// Concurrent reader: cursor must never decrease.
	stop := make(chan struct{})
	var lastCur atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, cur, _ := r.Read(lastCur.Load())
				prev := lastCur.Load()
				if cur < prev {
					t.Errorf("cursor went backwards: %d -> %d", prev, cur)
				}
				lastCur.Store(cur)
			}
		}
	}()

	wg.Wait()
	close(stop)

	// Final state is consistent.
	if r.Written()+r.Dropped() == 0 {
		t.Fatal("no writes recorded")
	}
}

func BenchmarkRingWrite1KB(b *testing.B) {
	r := NewRing(64 * 1024)
	p := bytes.Repeat([]byte{'x'}, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Write(p)
	}
}

func BenchmarkRingReadCatchUp(b *testing.B) {
	r := NewRing(64 * 1024)
	p := bytes.Repeat([]byte{'x'}, 1024)
	for i := 0; i < 16; i++ {
		r.Write(p)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, cur, _ := r.Read(0)
		_ = cur
	}
}
