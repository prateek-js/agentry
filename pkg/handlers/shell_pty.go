package handlers

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agentry-ai/agentry/pkg/shell"
	"github.com/gorilla/websocket"
)

// Binary frame type prefixes. The first byte of every binary
// WebSocket message identifies its payload, matching OpenSandbox's
// pty-ws protocol so existing tooling lines up.
const (
	FrameStdout = 0x01
	FrameStderr = 0x02 // reserved; we merge stderr into stdout via the pty
	FrameStdin  = 0x03
	FrameReplay = 0x04 // sent on attach with the recent transcript
	FrameExit   = 0x05
)

// Tunables for the WS layer.
const (
	ptyWSPingPeriod = 30 * time.Second
	ptyWSPongWait   = 60 * time.Second
	ptyWSWriteWait  = 10 * time.Second
)

// ptyUpgrader is shared across requests; it has no per-connection state.
// We don't validate Origin here — the auth middleware in the chain
// already gates access via API key.
var ptyUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// PTYWebSocketHandler upgrades the request to a WebSocket and binds it
// to a PTY session keyed by `?session_id=…`. Query params `rows` and
// `cols` set the initial window size (defaults 24x80).
//
//	Binary frames:
//	  client → server:  [0x03 | stdin payload]
//	  server → client:  [0x01 | stdout payload]
//	                    [0x04 | replay payload]   (sent once on attach)
//	                    [0x05 | int32 BE exit]
//
//	Text frames (control, JSON):
//	  {"type":"resize","rows":N,"cols":M}
func PTYWebSocketHandler(mgr *shell.PTYManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session_id")
		if sid == "" {
			sid = "default"
		}
		rows := parseDim(r.URL.Query().Get("rows"), 24)
		cols := parseDim(r.URL.Query().Get("cols"), 80)

		p, err := mgr.GetOrCreate(sid, rows, cols)
		if err != nil {
			Error(w, http.StatusServiceUnavailable, err.Error())
			return
		}

		conn, err := ptyUpgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade has already written the response.
			return
		}
		// runWebSocket owns the conn lifetime including Close.
		runWebSocket(r.Context(), conn, p)
	}
}

// PTYListHandler returns the set of live PTYs (id, attached state).
func PTYListHandler(mgr *shell.PTYManager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		Success(w, "ok", map[string]any{"ptys": mgr.List()})
	}
}

// PTYCloseHandler force-kills a PTY by id.
func PTYCloseHandler(mgr *shell.PTYManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !mgr.Close(id) {
			Error(w, http.StatusNotFound, "pty not found")
			return
		}
		Success(w, "closed", map[string]string{"id": id})
	}
}

// runWebSocket wires the upgraded conn to the pty, blocking until
// either side closes. It is the only thing that touches conn.
func runWebSocket(parent context.Context, conn *websocket.Conn, p *shell.PTY) {
	defer conn.Close()

	// Single writer mutex — gorilla/websocket allows only one writer at
	// a time. The output goroutine and the ping goroutine both grab it.
	writer := newWSWriter(conn)

	// Configure read side: bound message size, install pong handler so
	// the deadline slides forward on each pong.
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(ptyWSPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(ptyWSPongWait))
	})

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// 1) Attach: stream pty output to the WS until the pty exits or
	//    ctx is cancelled. AttachStream serializes WriteOutput so we
	//    can safely share the writer mutex with the ping goroutine.
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- p.AttachStream(ctx, writer)
	}()

	// 2) Ping loop: periodic pings keep the connection alive through
	//    NAT and detect half-open sockets.
	go func() {
		t := time.NewTicker(ptyWSPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writer.WritePing(); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// 3) Read loop: dispatch incoming frames to stdin / resize.
	// Returns when the client closes or sends garbage.
	readErr := wsReadLoop(conn, p, cancel)

	// Wait for the attach goroutine to drain so we know we've sent the
	// exit frame (if applicable) before tearing down.
	if err := attachDone; err != nil {
		select {
		case e := <-err:
			if e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, websocket.ErrCloseSent) {
				log.Printf("pty %s: stream ended: %v", "", e)
			}
		case <-time.After(2 * time.Second):
		}
	}
	_ = readErr // already used for cancellation
}

// wsReadLoop reads frames from the WS until the connection closes.
// Binary frames whose first byte is FrameStdin pipe to the pty; text
// frames are decoded as JSON control messages.
func wsReadLoop(conn *websocket.Conn, p *shell.PTY, cancel context.CancelFunc) error {
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			cancel()
			return err
		}
		switch msgType {
		case websocket.BinaryMessage:
			if len(data) < 1 {
				continue
			}
			switch data[0] {
			case FrameStdin:
				if err := p.WriteStdin(data[1:]); err != nil {
					cancel()
					return err
				}
			default:
				// Unknown binary type — silently ignore so older or
				// newer clients can coexist.
			}
		case websocket.TextMessage:
			if err := handleControlFrame(p, data); err != nil {
				// Bad JSON or unknown control — drop the connection so
				// the client sees a hard failure rather than silently
				// degrading.
				cancel()
				return err
			}
		}
	}
}

// controlFrame is the JSON envelope for text-frame control messages.
type controlFrame struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

func handleControlFrame(p *shell.PTY, data []byte) error {
	var cf controlFrame
	if err := json.Unmarshal(data, &cf); err != nil {
		return err
	}
	switch cf.Type {
	case "resize":
		return p.Resize(cf.Rows, cf.Cols)
	default:
		// Unknown control types are tolerated for forward-compat.
		return nil
	}
}

// wsWriter wraps a *websocket.Conn with a mutex (gorilla forbids
// concurrent writes) and implements shell.PTYWriter.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func newWSWriter(c *websocket.Conn) *wsWriter {
	return &wsWriter{conn: c}
}

// WriteOutput emits a stdout frame: [0x01 | payload].
func (w *wsWriter) WriteOutput(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(ptyWSWriteWait))
	// Allocate once so the framing prefix is contiguous with the
	// payload — saves a partial-write split on the wire.
	out := make([]byte, 1+len(p))
	out[0] = FrameStdout
	copy(out[1:], p)
	return w.conn.WriteMessage(websocket.BinaryMessage, out)
}

// WriteExit emits an exit frame: [0x05 | int32 BE exit code]. Best-effort
// — if the client has already gone away we swallow the error.
func (w *wsWriter) WriteExit(code int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(ptyWSWriteWait))
	frame := make([]byte, 5)
	frame[0] = FrameExit
	binary.BigEndian.PutUint32(frame[1:], uint32(int32(code)))
	if err := w.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return err
	}
	// Polite close frame so the peer's read loop wakes.
	_ = w.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "exit"))
	return nil
}

// WritePing sends an unsolicited ping. The peer's pong handler bumps
// the read deadline; if there's no pong within ptyWSPongWait the
// connection drops.
func (w *wsWriter) WritePing() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(ptyWSWriteWait))
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

func parseDim(s string, fallback uint16) uint16 {
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return fallback
	}
	return uint16(n)
}
