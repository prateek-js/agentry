//go:build !windows
// +build !windows

package jupyter

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireIPyKernel skips the test unless `python3 -m ipykernel` is
// runnable on this host. Cheap probe avoids the 10s readyTimeout when
// CI hasn't installed the package.
func requireIPyKernel(t *testing.T) {
	t.Helper()
	cmd := exec.Command("python3", "-c", "import ipykernel")
	if err := cmd.Run(); err != nil {
		t.Skipf("python3/ipykernel not available: %v", err)
	}
}

// runOnce starts a kernel, executes code, drains the iopub stream, and
// returns the concatenated stdout text plus the final reply. Closes
// the kernel before returning.
func runOnce(t *testing.T, code string) (string, *ExecuteReply) {
	t.Helper()
	requireIPyKernel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	k, err := StartKernel(context.Background(), ctx, "test", "python", KernelReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Shutdown(2 * time.Second) })

	return drainExecute(t, ctx, k, code)
}

// drainExecute is the test helper that runs one execute against an
// already-running kernel and returns concatenated stdout + final reply.
func drainExecute(t *testing.T, ctx context.Context, k *Kernel, code string) (string, *ExecuteReply) {
	t.Helper()
	stream, err := k.Execute(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var stdout strings.Builder
	var idleSeen bool
	var reply *ExecuteReply

	deadline := time.After(10 * time.Second)
	for !idleSeen || reply == nil {
		select {
		case <-deadline:
			t.Fatalf("timed out: idle=%v reply=%v stdout=%q", idleSeen, reply, stdout.String())
		case m, ok := <-stream.IOPub:
			if !ok {
				idleSeen = true
				continue
			}
			switch m.MsgType() {
			case "stream":
				var c StreamContent
				_ = m.DecodeContent(&c)
				if c.Name == "stdout" {
					stdout.WriteString(c.Text)
				}
			case "status":
				var s StatusContent
				_ = m.DecodeContent(&s)
				if s.ExecutionState == "idle" && m.ParentMsgID() == stream.MsgID {
					idleSeen = true
				}
			}
		case m, ok := <-stream.Shell:
			if !ok {
				continue
			}
			var r ExecuteReply
			_ = m.DecodeContent(&r)
			reply = &r
		}
	}
	return stdout.String(), reply
}

func TestKernelPrintsHello(t *testing.T) {
	out, reply := runOnce(t, `print("hello-from-kernel")`)
	if !strings.Contains(out, "hello-from-kernel") {
		t.Errorf("stdout = %q; want substring 'hello-from-kernel'", out)
	}
	if reply == nil || reply.Status != "ok" {
		t.Errorf("reply = %+v; want status=ok", reply)
	}
}

func TestKernelStatePersistsAcrossCalls(t *testing.T) {
	requireIPyKernel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	k, err := StartKernel(context.Background(), ctx, "stateful", "python", KernelReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Shutdown(2 * time.Second) })

	// Set a variable.
	_, r1 := drainExecute(t, ctx, k, "x = 42")
	if r1.Status != "ok" {
		t.Fatalf("set-var reply: %+v", r1)
	}
	// Print it back in a SECOND execute — must persist.
	out, r2 := drainExecute(t, ctx, k, "print(x * 10)")
	if r2.Status != "ok" {
		t.Fatalf("read-var reply: %+v", r2)
	}
	if !strings.Contains(out, "420") {
		t.Errorf("expected '420' in output; got %q", out)
	}
}

func TestKernelExceptionTrace(t *testing.T) {
	requireIPyKernel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	k, err := StartKernel(context.Background(), ctx, "err", "python", KernelReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Shutdown(2 * time.Second) })

	stream, err := k.Execute(ctx, "raise ValueError('boom')")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var (
		gotErr  bool
		reply   *ExecuteReply
		idleHit bool
	)
	deadline := time.After(10 * time.Second)
	for !idleHit || reply == nil {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for error+reply")
		case m, ok := <-stream.IOPub:
			if !ok {
				idleHit = true
				continue
			}
			switch m.MsgType() {
			case "error":
				var e ErrorContent
				_ = m.DecodeContent(&e)
				if e.ENAME != "ValueError" {
					t.Errorf("ENAME = %s; want ValueError", e.ENAME)
				}
				gotErr = true
			case "status":
				var s StatusContent
				_ = m.DecodeContent(&s)
				if s.ExecutionState == "idle" && m.ParentMsgID() == stream.MsgID {
					idleHit = true
				}
			}
		case m, ok := <-stream.Shell:
			if !ok {
				continue
			}
			var r ExecuteReply
			_ = m.DecodeContent(&r)
			reply = &r
		}
	}
	if !gotErr {
		t.Error("never received an error iopub message")
	}
	if reply.Status != "error" {
		t.Errorf("reply.Status = %s; want error", reply.Status)
	}
}

func TestKernelExecuteResult(t *testing.T) {
	requireIPyKernel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	k, err := StartKernel(context.Background(), ctx, "result", "python", KernelReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Shutdown(2 * time.Second) })

	stream, err := k.Execute(ctx, "7 * 6")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var (
		resultData string
		idle       bool
		reply      *ExecuteReply
	)
	deadline := time.After(10 * time.Second)
	for !idle || reply == nil {
		select {
		case <-deadline:
			t.Fatal("timeout")
		case m, ok := <-stream.IOPub:
			if !ok {
				idle = true
				continue
			}
			if m.MsgType() == "execute_result" {
				var r ExecuteResultContent
				_ = m.DecodeContent(&r)
				if txt, ok := r.Data["text/plain"].(string); ok {
					resultData = txt
				}
			}
			if m.MsgType() == "status" {
				var s StatusContent
				_ = m.DecodeContent(&s)
				if s.ExecutionState == "idle" && m.ParentMsgID() == stream.MsgID {
					idle = true
				}
			}
		case m, ok := <-stream.Shell:
			if !ok {
				continue
			}
			var r ExecuteReply
			_ = m.DecodeContent(&r)
			reply = &r
		}
	}
	if resultData != "42" {
		t.Errorf("execute_result = %q; want 42", resultData)
	}
}

func TestManagerSpawnAndExecute(t *testing.T) {
	requireIPyKernel(t)
	m := NewManager()
	t.Cleanup(m.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	k, err := m.Spawn(ctx, "ctx-1", "python")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("ctx-1"); got != k {
		t.Errorf("Get returned a different kernel")
	}
	if list := m.List(); len(list) != 1 || list[0].ID != "ctx-1" || list[0].Language != "python" {
		t.Errorf("List = %+v", list)
	}

	// Verify the kernel works via the manager-issued handle.
	out, _ := drainExecute(t, ctx, k, "print('via-manager')")
	if !strings.Contains(out, "via-manager") {
		t.Errorf("stdout = %q", out)
	}

	if err := m.Shutdown("ctx-1"); err != nil {
		t.Fatal(err)
	}
	// After shutdown, the goroutine clean-up may take a beat.
	time.Sleep(100 * time.Millisecond)
	if _, err := m.Get("ctx-1"); err == nil {
		t.Errorf("Get should fail after shutdown")
	}
}

func TestManagerRejectsDuplicateID(t *testing.T) {
	requireIPyKernel(t)
	m := NewManager()
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := m.Spawn(ctx, "dup", "python"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Spawn(ctx, "dup", "python"); err == nil {
		t.Fatal("expected duplicate-id error")
	}
}

// TestPythonInitCodeEnablesInlineBackend is the proof-of-warmup: spawn
// a Python kernel and immediately run `plt.show()` WITHOUT touching
// `%matplotlib inline`. With the auto-init wired in pythonSpec, the
// kernel should emit an image/png display_data event. Skips when
// matplotlib isn't available (most CI hosts won't have it pre-installed;
// the runtime image does).
func TestPythonInitCodeEnablesInlineBackend(t *testing.T) {
	requireIPyKernel(t)
	if err := exec.Command("python3", "-c", "import matplotlib").Run(); err != nil {
		t.Skip("matplotlib not available on this host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k, err := StartKernel(context.Background(), ctx, "inline-init", "python", KernelReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Shutdown(2 * time.Second) })

	stream, err := k.Execute(ctx,
		"import matplotlib.pyplot as plt; plt.figure(); plt.plot([1,2,3]); plt.show()")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	// Drain until we see a display_data with image/png OR the reply
	// arrives without one (the failure case).
	deadline := time.After(15 * time.Second)
	var gotPNG bool
	var idle, replyArrived bool
	for !idle || !replyArrived {
		select {
		case <-deadline:
			t.Fatalf("timeout; gotPNG=%v idle=%v reply=%v", gotPNG, idle, replyArrived)
		case m, ok := <-stream.IOPub:
			if !ok {
				idle = true
				continue
			}
			if m.MsgType() == "display_data" {
				var c DisplayDataContent
				_ = m.DecodeContent(&c)
				if _, hasPNG := c.Data["image/png"]; hasPNG {
					gotPNG = true
				}
			}
			if m.MsgType() == "status" {
				var s StatusContent
				_ = m.DecodeContent(&s)
				if s.ExecutionState == "idle" && m.ParentMsgID() == stream.MsgID {
					idle = true
				}
			}
		case _, ok := <-stream.Shell:
			if !ok {
				continue
			}
			replyArrived = true
		}
	}
	if !gotPNG {
		t.Fatal("plt.show() emitted no image/png — warmup did not enable the inline backend")
	}
}

func TestKernelUnknownLanguage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := StartKernel(context.Background(), ctx, "x", "fortran", KernelReadyTimeout); err == nil {
		t.Fatal("expected unknown-language error")
	}
}
