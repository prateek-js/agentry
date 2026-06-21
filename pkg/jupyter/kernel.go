package jupyter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentry-ai/agentry/pkg/shell"
	"github.com/go-zeromq/zmq4"
)

// LanguageSpec describes how to spawn a kernel for one language. The
// stock entry below covers Python via ipykernel; we can add IJava,
// ijavascript, etc. by registering new specs.
type LanguageSpec struct {
	// Name is the user-facing identifier (e.g. "python", "node").
	Name string
	// Command + Args are how to launch the kernel. The literal
	// "{connection_file}" in any arg is substituted with the actual
	// path at spawn time.
	Command string
	Args    []string

	// InitCode is executed silently in the kernel immediately after
	// it answers kernel_info_request and BEFORE the context becomes
	// reachable through Execute. Use it to set sensible defaults the
	// user shouldn't have to remember (e.g. enabling the inline
	// matplotlib backend in Python). Failures are tolerated — a
	// kernel that boots but can't run the init still proceeds; the
	// only cost is the user has to set those defaults themselves.
	InitCode string
}

// pythonSpec is the default. Equivalent to `python3 -m ipykernel_launcher`,
// plus an init step that wires the inline matplotlib backend so
// `plt.show()` emits a PNG display_data event without the caller
// having to type `%matplotlib inline` first.
var pythonSpec = LanguageSpec{
	Name:    "python",
	Command: "python3",
	Args:    []string{"-m", "ipykernel_launcher", "-f", "{connection_file}"},
	InitCode: `try:
    get_ipython().run_line_magic('matplotlib', 'inline')
except Exception:
    pass
`,
}

// builtin is the seed registry of known kernels. Extra entries can be
// registered at runtime via RegisterLanguage.
var (
	langMu  sync.RWMutex
	langReg = map[string]LanguageSpec{
		"python":  pythonSpec,
		"python3": pythonSpec,
	}
)

// RegisterLanguage adds (or overrides) a kernel spec. Safe for
// concurrent use.
func RegisterLanguage(spec LanguageSpec) {
	langMu.Lock()
	defer langMu.Unlock()
	langReg[spec.Name] = spec
}

func lookupLanguage(name string) (LanguageSpec, bool) {
	langMu.RLock()
	defer langMu.RUnlock()
	s, ok := langReg[name]
	return s, ok
}

// Kernel is one running Jupyter kernel process plus the ZMQ sockets
// used to talk to it. It is safe for concurrent calls into Execute and
// Shutdown; iopub fan-out is serialized through iopubRouter.
type Kernel struct {
	id       string
	language string

	conn   *ConnectionFile
	cmd    *exec.Cmd
	connFP string // path to the connection file we wrote

	shell   zmq4.Socket
	iopub   zmq4.Socket
	control zmq4.Socket
	stdinSk zmq4.Socket
	hb      zmq4.Socket
	shellMu sync.Mutex // serialize shell sends; ipykernel handles one at a time anyway

	keyBytes []byte
	session  string

	router *iopubRouter
	once   sync.Once
	closed atomic.Bool
	done   chan struct{}

	// stderrTail keeps the kernel's recent stderr around for surfacing
	// in error messages — invaluable when the kernel exits during
	// startup (e.g. ipykernel not installed).
	stderrMu   sync.Mutex
	stderrTail []byte
	stderrCap  int
}

// StartKernel spawns a kernel for the given language and waits for it
// to become responsive. Returns a fully wired Kernel ready to Execute.
//
// lifeCtx outlives the kernel — it binds the process and ZMQ sockets.
// readyCtx is the cold-start wait deadline only; it may be canceled by
// the caller (e.g. when an HTTP request returns) without killing the
// kernel.
//
// readyTimeout bounds the kernel-boot wait. 10s is generous for
// ipykernel cold start.
func StartKernel(lifeCtx, readyCtx context.Context, id, language string, readyTimeout time.Duration) (*Kernel, error) {
	spec, ok := lookupLanguage(language)
	if !ok {
		return nil, fmt.Errorf("unknown language %q", language)
	}
	conn, err := NewConnectionFile(language)
	if err != nil {
		return nil, err
	}
	connFP, err := conn.WriteTo("")
	if err != nil {
		return nil, err
	}

	args := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		if a == "{connection_file}" {
			args[i] = connFP
		} else {
			args[i] = a
		}
	}

	// The process and the ZMQ sockets are bound to lifeCtx — the
	// caller controls when the kernel actually dies.
	cmd := exec.CommandContext(lifeCtx, spec.Command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Inherit a login-shell env so operator-staged bindings
	// (/etc/profile.d/sandbox-creds.sh → TRINO_URL etc.) reach the
	// kernel. Without this, the kernel only sees the runtime
	// daemon's env from BEFORE any binding was applied.
	cmd.Env = shell.LoginShellEnv(lifeCtx)

	k := &Kernel{
		id:        id,
		language:  language,
		conn:      conn,
		cmd:       cmd,
		connFP:    connFP,
		keyBytes:  []byte(conn.Key),
		session:   mustRandomID(),
		router:    newIOPubRouter(),
		done:      make(chan struct{}),
		stderrCap: 8 * 1024,
	}

	// Pipe stderr into a tailing writer so we can show it on errors.
	cmd.Stderr = &boundedWriter{k: k}
	cmd.Stdout = &boundedWriter{k: k}

	if err := cmd.Start(); err != nil {
		_ = os.Remove(connFP)
		return nil, fmt.Errorf("start kernel: %w", err)
	}

	if err := k.connectSockets(lifeCtx); err != nil {
		_ = k.killNow()
		return nil, err
	}
	go k.iopubLoop()
	go k.processWaitLoop()

	if err := k.waitReady(readyCtx, readyTimeout); err != nil {
		_ = k.Shutdown(2 * time.Second)
		return nil, err
	}
	// Optional one-shot warmup. Errors here are tolerated — the kernel
	// is still usable, just without whatever default the spec wanted.
	if spec.InitCode != "" {
		_ = k.runSilent(readyCtx, spec.InitCode, 5*time.Second)
	}
	return k, nil
}

// runSilent sends one execute_request with silent=true,
// store_history=false (so the cell doesn't bump execution_count or
// leak iopub stream events to subsequent execute callers) and waits
// for the matching execute_reply on the shell socket. Used by
// StartKernel for per-language warmup; not exposed externally.
func (k *Kernel) runSilent(ctx context.Context, code string, timeout time.Duration) error {
	if k.closed.Load() {
		return errors.New("kernel: closed")
	}
	msgID := mustRandomID()

	body, err := json.Marshal(ExecuteRequest{
		Code:            code,
		Silent:          true,
		StoreHistory:    false,
		UserExpressions: map[string]any{},
		AllowStdin:      false,
		StopOnError:     false,
	})
	if err != nil {
		return err
	}
	req := &Message{
		Header: Header{
			MsgID:    msgID,
			Session:  k.session,
			Username: "ad-sandbox",
			MsgType:  "execute_request",
		},
		Content: body,
	}
	if err := k.sendShell(req); err != nil {
		return err
	}

	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		frames, err := k.recvShell(deadline)
		if err != nil {
			return err
		}
		reply, perr := ParseMessage(frames, k.keyBytes)
		if perr != nil {
			continue
		}
		if reply.ParentMsgID() == msgID && reply.MsgType() == "execute_reply" {
			return nil
		}
	}
}

// connectSockets dials all five Jupyter ZMQ endpoints and subscribes
// the iopub socket to every topic.
func (k *Kernel) connectSockets(ctx context.Context) error {
	shell := zmq4.NewDealer(ctx)
	iopub := zmq4.NewSub(ctx)
	stdinSk := zmq4.NewDealer(ctx)
	control := zmq4.NewDealer(ctx)
	hb := zmq4.NewReq(ctx)

	for _, p := range []struct {
		name  string
		sock  zmq4.Socket
		endpt string
	}{
		{"shell", shell, k.conn.Endpoint("shell")},
		{"iopub", iopub, k.conn.Endpoint("iopub")},
		{"stdin", stdinSk, k.conn.Endpoint("stdin")},
		{"control", control, k.conn.Endpoint("control")},
		{"hb", hb, k.conn.Endpoint("hb")},
	} {
		if err := p.sock.Dial(p.endpt); err != nil {
			// Best-effort close of anything we've already dialed.
			_ = shell.Close()
			_ = iopub.Close()
			_ = stdinSk.Close()
			_ = control.Close()
			_ = hb.Close()
			return fmt.Errorf("dial %s @ %s: %w", p.name, p.endpt, err)
		}
	}
	// SUB sockets need an explicit topic filter; empty matches all.
	if err := iopub.SetOption(zmq4.OptionSubscribe, ""); err != nil {
		return fmt.Errorf("iopub subscribe: %w", err)
	}

	k.shell = shell
	k.iopub = iopub
	k.stdinSk = stdinSk
	k.control = control
	k.hb = hb
	return nil
}

// waitReady spins a kernel_info_request against shell until the kernel
// answers. ipykernel is responsive within ~200ms on a warm box, longer
// on a cold container. Times out after deadline.
func (k *Kernel) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	probeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for {
		if probeCtx.Err() != nil {
			return fmt.Errorf("kernel not ready within %s: %s", timeout, k.lastStderr())
		}
		// Use a generous per-probe timeout — early probes will sit
		// idle waiting for ipykernel to bind its sockets.
		ok, err := k.probeKernelInfo(probeCtx, 1*time.Second)
		if err == nil && ok {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("kernel not ready within %s: %s", timeout, k.lastStderr())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (k *Kernel) probeKernelInfo(ctx context.Context, perCall time.Duration) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, perCall)
	defer cancel()

	msgID := mustRandomID()
	req := &Message{
		Header: Header{
			MsgID:    msgID,
			Session:  k.session,
			Username: "ad-sandbox",
			MsgType:  "kernel_info_request",
		},
		Content: []byte("{}"),
	}
	if err := k.sendShell(req); err != nil {
		return false, err
	}
	// Read shell replies until we see one whose parent matches.
	for {
		frames, err := k.recvShell(probeCtx)
		if err != nil {
			return false, err
		}
		reply, err := ParseMessage(frames, k.keyBytes)
		if err != nil {
			continue
		}
		if reply.ParentMsgID() == msgID && reply.MsgType() == "kernel_info_reply" {
			return true, nil
		}
	}
}

// iopubLoop pumps SUB messages into the router until the kernel exits
// or the socket errors. Single goroutine — the router does the fan-out.
func (k *Kernel) iopubLoop() {
	for {
		if k.closed.Load() {
			return
		}
		zmsg, err := k.iopub.Recv()
		if err != nil {
			return
		}
		m, perr := ParseMessage(zmsg.Frames, k.keyBytes)
		if perr != nil {
			// Bad HMAC or framing — log once and keep going.
			continue
		}
		k.router.Dispatch(m)
	}
}

// processWaitLoop blocks on cmd.Wait and tears down on exit.
func (k *Kernel) processWaitLoop() {
	err := k.cmd.Wait()
	_ = err // surfaced via lastStderr / closed state
	k.closed.Store(true)
	k.router.CloseAll()
	close(k.done)
	_ = os.Remove(k.connFP)
}

// Execute sends `code` to the kernel and returns a channel that emits
// every iopub message whose parent is this request, followed by a
// final shell reply summary. The channel is closed when the kernel
// signals status=idle (or ctx fires).
//
// Backpressure: the per-execute channel is buffered (256 by default);
// overflow drops messages and the returned `dropped` callback reports
// the total. We avoid blocking the kernel's iopub stream — that would
// pile up across other requests.
func (k *Kernel) Execute(ctx context.Context, code string) (*ExecuteStream, error) {
	if k.closed.Load() {
		return nil, errors.New("kernel: closed")
	}
	msgID := mustRandomID()

	ch, unsub := k.router.Subscribe(msgID, 256)

	req := &Message{
		Header: Header{
			MsgID:    msgID,
			Session:  k.session,
			Username: "ad-sandbox",
			MsgType:  "execute_request",
		},
	}
	body, err := json.Marshal(ExecuteRequest{
		Code:            code,
		Silent:          false,
		StoreHistory:    true,
		UserExpressions: map[string]any{},
		AllowStdin:      false,
		StopOnError:     true,
	})
	if err != nil {
		unsub()
		return nil, err
	}
	req.Content = body

	if err := k.sendShell(req); err != nil {
		unsub()
		return nil, err
	}

	// Read execute_reply asynchronously so the iopub stream can flow in
	// parallel with the shell-side return.
	replyCh := make(chan *Message, 1)
	go func() {
		defer close(replyCh)
		for {
			frames, rerr := k.recvShell(ctx)
			if rerr != nil {
				return
			}
			reply, perr := ParseMessage(frames, k.keyBytes)
			if perr != nil {
				continue
			}
			if reply.ParentMsgID() == msgID {
				replyCh <- reply
				return
			}
		}
	}()

	stream := &ExecuteStream{
		MsgID: msgID,
		IOPub: ch,
		Shell: replyCh,
		unsub: unsub,
	}
	return stream, nil
}

// ExecuteStream is what Execute returns. Consumers select on IOPub
// (status/stream/result/error) until they see a status=idle whose
// parent is MsgID, then read Shell for the final reply, then call
// Close to release the iopub subscription.
type ExecuteStream struct {
	MsgID string
	IOPub <-chan *Message
	Shell <-chan *Message

	unsub func() int64
	once  sync.Once
}

// Close releases the iopub subscription and returns the number of
// iopub messages that were dropped because the consumer fell behind.
func (s *ExecuteStream) Close() int64 {
	var dropped int64
	s.once.Do(func() {
		dropped = s.unsub()
	})
	return dropped
}

// Interrupt sends an interrupt_request on the control socket. The
// kernel raises KeyboardInterrupt in whatever execution is in flight.
func (k *Kernel) Interrupt(ctx context.Context) error {
	if k.closed.Load() {
		return errors.New("kernel: closed")
	}
	msg := &Message{
		Header: Header{
			MsgID:    mustRandomID(),
			Session:  k.session,
			Username: "ad-sandbox",
			MsgType:  "interrupt_request",
		},
		Content: []byte("{}"),
	}
	parts, err := msg.Marshal(k.keyBytes)
	if err != nil {
		return err
	}
	return k.control.Send(zmq4.NewMsgFrom(parts...))
}

// Shutdown asks the kernel to stop, waits up to `grace`, then SIGKILLs
// the process group as a fallback. Always idempotent.
func (k *Kernel) Shutdown(grace time.Duration) error {
	if k.closed.Load() {
		return nil
	}
	// Send shutdown_request on control. ipykernel honors it within
	// a few hundred ms.
	msg := &Message{
		Header: Header{
			MsgID:    mustRandomID(),
			Session:  k.session,
			Username: "ad-sandbox",
			MsgType:  "shutdown_request",
		},
		Content: []byte(`{"restart":false}`),
	}
	parts, err := msg.Marshal(k.keyBytes)
	if err == nil {
		_ = k.control.Send(zmq4.NewMsgFrom(parts...))
	}

	select {
	case <-k.done:
	case <-time.After(grace):
	}
	return k.killNow()
}

// IsClosed reports whether the kernel has exited.
func (k *Kernel) IsClosed() bool { return k.closed.Load() }

// Language returns the kernel's language name (informational).
func (k *Kernel) Language() string { return k.language }

// ID returns the manager-assigned kernel id.
func (k *Kernel) ID() string { return k.id }

// killNow does the irrecoverable teardown. Idempotent.
func (k *Kernel) killNow() error {
	k.once.Do(func() {
		k.closed.Store(true)
		if k.cmd != nil && k.cmd.Process != nil {
			_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _ = k.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		closeSockets(k.shell, k.iopub, k.stdinSk, k.control, k.hb)
		_ = os.Remove(k.connFP)
	})
	return nil
}

func closeSockets(socks ...zmq4.Socket) {
	for _, s := range socks {
		if s != nil {
			_ = s.Close()
		}
	}
}

// sendShell signs and writes a request to the kernel's shell socket.
// Serialized to keep the wire ordering deterministic — ipykernel
// processes shell messages one at a time anyway.
func (k *Kernel) sendShell(m *Message) error {
	parts, err := m.Marshal(k.keyBytes)
	if err != nil {
		return err
	}
	k.shellMu.Lock()
	defer k.shellMu.Unlock()
	return k.shell.Send(zmq4.NewMsgFrom(parts...))
}

// recvShell blocks until ctx fires or a frameful message arrives. The
// zmq4 Socket has no ctx-aware Recv, so we run the Recv in a goroutine
// and select against ctx.
func (k *Kernel) recvShell(ctx context.Context) ([][]byte, error) {
	type res struct {
		frames [][]byte
		err    error
	}
	out := make(chan res, 1)
	go func() {
		m, err := k.shell.Recv()
		out <- res{m.Frames, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-out:
		return r.frames, r.err
	}
}

// boundedWriter captures the kernel's recent stdout/stderr into a
// fixed-size tail buffer. We surface it in error messages when the
// kernel refuses to come up (most common cause: ipykernel not
// installed in the runtime image).
type boundedWriter struct {
	k *Kernel
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.k.stderrMu.Lock()
	defer w.k.stderrMu.Unlock()
	w.k.stderrTail = append(w.k.stderrTail, p...)
	if extra := len(w.k.stderrTail) - w.k.stderrCap; extra > 0 {
		w.k.stderrTail = w.k.stderrTail[extra:]
	}
	return len(p), nil
}

func (k *Kernel) lastStderr() string {
	k.stderrMu.Lock()
	defer k.stderrMu.Unlock()
	s := string(k.stderrTail)
	if s == "" {
		return "(kernel stderr empty)"
	}
	return s
}

// mustRandomID returns a 32-hex-char id. Panics on rand failure —
// that signals the OS is in a much worse state than we can recover
// from here, and exec already requires entropy elsewhere.
func mustRandomID() string {
	v, err := randomKey(16)
	if err != nil {
		log.Panicf("randomKey: %v", err)
	}
	return v
}
