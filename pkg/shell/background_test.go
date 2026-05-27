package shell

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitForStatus polls a command until it reaches one of the desired states
// or the deadline elapses. Returns the final BgStatus snapshot.
func waitForStatus(t *testing.T, m *BackgroundManager, id string, want ...string) BgStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := m.Status(id)
		if !ok {
			t.Fatalf("command %s vanished", id)
		}
		for _, w := range want {
			if s.Status == w {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; current statuses=%v", id, want)
	return BgStatus{}
}

func TestBackgroundShortCommandCompletes(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	id, err := m.Start(context.Background(), "echo hello && echo world", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := waitForStatus(t, m, id, BgStatusCompleted, BgStatusFailed)
	if st.Status != BgStatusCompleted {
		t.Fatalf("status = %s err=%s", st.Status, st.Error)
	}
	if st.ExitCode != 0 {
		t.Errorf("exit_code = %d; want 0", st.ExitCode)
	}

	data, cur, dropped, ok := m.Logs(id, 0)
	if !ok {
		t.Fatal("logs not found")
	}
	if dropped != 0 {
		t.Errorf("dropped = %d for tiny output", dropped)
	}
	if cur <= 0 {
		t.Errorf("cursor = %d; want > 0", cur)
	}
	if !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "world") {
		t.Errorf("logs missing expected output: %q", data)
	}
}

func TestBackgroundFailingCommandCapturesExitCode(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	id, err := m.Start(context.Background(), "sh -c 'echo bye; exit 7'", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := waitForStatus(t, m, id, BgStatusFailed, BgStatusCompleted)
	if st.Status != BgStatusFailed {
		t.Fatalf("status = %s; want failed", st.Status)
	}
	if st.ExitCode != 7 {
		t.Errorf("exit_code = %d; want 7", st.ExitCode)
	}
}

func TestBackgroundIncrementalLogReads(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	// Slow trickle so we can observe partial reads.
	id, err := m.Start(context.Background(),
		`for i in 1 2 3 4 5; do echo line$i; sleep 0.05; done`, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	var seen []byte
	var cursor int64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, cur, _, ok := m.Logs(id, cursor)
		if !ok {
			t.Fatal("logs not found")
		}
		if len(data) > 0 {
			seen = append(seen, data...)
		}
		cursor = cur

		st, _ := m.Status(id)
		if st.Status != BgStatusRunning {
			// Final drain to catch tail bytes.
			data, cur, _, _ := m.Logs(id, cursor)
			seen = append(seen, data...)
			cursor = cur
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	for i := 1; i <= 5; i++ {
		want := "line" + string(rune('0'+i))
		if !strings.Contains(string(seen), want) {
			t.Errorf("transcript missing %q: %q", want, seen)
		}
	}
}

func TestBackgroundInterrupt(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	id, err := m.Start(context.Background(), "sleep 30", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm it's running.
	st, _ := m.Status(id)
	if st.Status != BgStatusRunning {
		t.Fatalf("status = %s; want running", st.Status)
	}
	if err := m.Interrupt(id); err != nil {
		t.Fatal(err)
	}
	st = waitForStatus(t, m, id, BgStatusInterrupted, BgStatusCompleted, BgStatusFailed)
	if st.Status != BgStatusInterrupted {
		t.Errorf("status after interrupt = %s; want interrupted", st.Status)
	}
}

func TestBackgroundInterruptUnknown(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()
	err := m.Interrupt("nope")
	if err == nil || !IsNotFound(err) {
		t.Fatalf("got %v; want not-found", err)
	}
}

func TestBackgroundList(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()
	id1, _ := m.Start(context.Background(), "echo a", "", nil)
	id2, _ := m.Start(context.Background(), "echo b", "", nil)
	waitForStatus(t, m, id1, BgStatusCompleted)
	waitForStatus(t, m, id2, BgStatusCompleted)

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("len = %d; want 2", len(list))
	}
}

func TestBackgroundForgetRequiresFinished(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	id, _ := m.Start(context.Background(), "sleep 30", "", nil)
	if ok := m.Forget(id); ok {
		t.Fatal("Forget should refuse to drop a running command")
	}
	_ = m.Interrupt(id)
	waitForStatus(t, m, id, BgStatusInterrupted)
	if ok := m.Forget(id); !ok {
		t.Fatal("Forget should succeed once stopped")
	}
	if _, ok := m.Status(id); ok {
		t.Fatal("Status should miss after Forget")
	}
}

func TestBackgroundGCExpiresFinished(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()
	id, _ := m.Start(context.Background(), "true", "", nil)
	waitForStatus(t, m, id, BgStatusCompleted)

	// Future timestamp far past the retention window.
	future := time.Now().Add(BgRetentionAfterFinish + time.Second)
	reaped := m.gcOnce(future)
	if reaped == 0 {
		t.Fatalf("gc did not reap finished command")
	}
	if _, ok := m.Status(id); ok {
		t.Fatal("command still present after gc")
	}
}

func TestBackgroundGCKeepsRunning(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()
	id, _ := m.Start(context.Background(), "sleep 30", "", nil)
	defer m.Interrupt(id)

	if r := m.gcOnce(time.Now().Add(time.Hour)); r != 0 {
		t.Errorf("gc reaped %d running commands", r)
	}
}

func TestBackgroundRingDropsAreSurfaced(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	// Generate > BgRingSize bytes. yes spews quickly; cap with head -c.
	id, err := m.Start(context.Background(),
		"yes a | head -c 2097152", "", nil) // 2 MiB > 1 MiB ring
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, BgStatusCompleted)

	st, _ := m.Status(id)
	if st.BytesDropped <= 0 {
		t.Errorf("expected drops > 0, got %d (BytesOut=%d)", st.BytesDropped, st.BytesOut)
	}
	if st.BytesOut <= int64(BgRingSize) {
		t.Errorf("BytesOut = %d; want >= ring size", st.BytesOut)
	}
}

func TestBackgroundStartContextCancelled(t *testing.T) {
	m := NewBackgroundManager()
	defer m.Shutdown()

	// Saturate the semaphore so subsequent Start blocks.
	saturate := make([]string, 0, BgMaxConcurrent)
	for i := 0; i < BgMaxConcurrent; i++ {
		id, err := m.Start(context.Background(), "sleep 30", "", nil)
		if err != nil {
			t.Fatalf("setup Start: %v", err)
		}
		saturate = append(saturate, id)
	}
	t.Cleanup(func() {
		for _, id := range saturate {
			_ = m.Interrupt(id)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := m.Start(ctx, "echo nope", "", nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline-exceeded, got %v", err)
	}
}

func TestBackgroundShutdownInterruptsRunning(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start(context.Background(), "sleep 30", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Shutdown()
	st := waitForStatus(t, m, id, BgStatusInterrupted, BgStatusFailed, BgStatusCompleted)
	if st.Status == BgStatusRunning {
		t.Fatalf("Shutdown did not interrupt running command (status=%s)", st.Status)
	}
}
