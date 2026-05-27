package jupyter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// delimiter is the fixed sentinel between optional ZMQ identity frames
// and the signed payload frames. Every Jupyter message includes it.
const delimiter = "<IDS|MSG>"

// jupyterVersion is the protocol version we advertise. 5.3 has been
// stable since 2017; ipykernel speaks it natively.
const jupyterVersion = "5.3"

// Header is the fixed fields every Jupyter message carries. The kernel
// echoes header fields back as parent_header on iopub messages so
// frontends can correlate.
type Header struct {
	MsgID    string `json:"msg_id"`
	Session  string `json:"session"`
	Username string `json:"username"`
	Date     string `json:"date"`
	MsgType  string `json:"msg_type"`
	Version  string `json:"version"`
}

// Message is one decoded Jupyter wire message. The four payload sections
// are kept as raw JSON so the protocol can evolve without us touching
// every field — the typed accessors below handle the common cases.
type Message struct {
	// Identities are the ZMQ routing prefix preserved from the wire.
	// On send to a ROUTER socket via DEALER they're not needed; on
	// iopub PUB→SUB the topic frame lives here as a single element.
	Identities [][]byte

	Header       Header
	ParentHeader Header // zero when no parent
	Metadata     json.RawMessage
	Content      json.RawMessage
	Buffers      [][]byte
}

// MsgType is a convenience accessor.
func (m *Message) MsgType() string { return m.Header.MsgType }

// ParentMsgID returns the parent header's msg_id (or "" when there's no
// parent). Used by iopubRouter to fan-out messages to the originating
// execute request.
func (m *Message) ParentMsgID() string { return m.ParentHeader.MsgID }

// DecodeContent unmarshals m.Content into v.
func (m *Message) DecodeContent(v any) error {
	if len(m.Content) == 0 {
		return nil
	}
	return json.Unmarshal(m.Content, v)
}

// --- buffer pools -------------------------------------------------------
//
// Jupyter messages are tiny (a few KB at most for iopub) but they come
// in at high rate during chatty executions (`for i in range(N): print(i)`
// will produce N stream messages). Pool the per-message scratch buffers
// to keep zero GC pressure in the steady state.

// hmacScratch holds the concatenated 4 JSON parts during HMAC
// computation. 1 KiB covers the common case; the slice grows
// transparently when needed.
//
// We deliberately do NOT pool the hash.Hash — hmac.New requires a
// constructor that yields a *fresh* hash on every call (it instantiates
// twice internally for inner/outer blocks). Pooling the constructor's
// result breaks the algorithm. The sha256.New allocation per message
// is cheap relative to the wire-level I/O so we accept it.
var hmacScratch = sync.Pool{
	New: func() any { b := make([]byte, 0, 1024); return &b },
}

// --- marshal ------------------------------------------------------------

// Marshal serializes m into a multipart ZMTP message ready to send.
// The returned slices include identities (if any), the delimiter, the
// HMAC signature, and the 4 payload JSON parts followed by buffers.
//
// Caller passes the kernel's signing key (raw bytes from
// ConnectionFile.Key, hex-decoded? — no, Jupyter uses the key as a
// utf-8 string by default; ipykernel's HMAC implementation does too).
func (m *Message) Marshal(key []byte) ([][]byte, error) {
	// Stamp dynamic header fields here so callers don't have to.
	if m.Header.Date == "" {
		m.Header.Date = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if m.Header.Version == "" {
		m.Header.Version = jupyterVersion
	}

	headerJSON, err := marshalCompact(m.Header)
	if err != nil {
		return nil, fmt.Errorf("encode header: %w", err)
	}
	parentJSON, err := marshalCompact(m.ParentHeader)
	if err != nil {
		return nil, fmt.Errorf("encode parent: %w", err)
	}
	metaJSON := m.Metadata
	if len(metaJSON) == 0 {
		metaJSON = []byte("{}")
	}
	contentJSON := m.Content
	if len(contentJSON) == 0 {
		contentJSON = []byte("{}")
	}

	sig := sign(key, headerJSON, parentJSON, metaJSON, contentJSON)

	parts := make([][]byte, 0, len(m.Identities)+6+len(m.Buffers))
	parts = append(parts, m.Identities...)
	parts = append(parts,
		[]byte(delimiter),
		[]byte(sig),
		headerJSON,
		parentJSON,
		metaJSON,
		contentJSON,
	)
	parts = append(parts, m.Buffers...)
	return parts, nil
}

// marshalCompact emits a Header (or zero Header) as canonical JSON
// without the extra whitespace that json.Marshal adds for struct
// encoding. Empty headers serialize to `{}` so signature verification
// stays stable across send/receive.
func marshalCompact(h Header) ([]byte, error) {
	if h == (Header{}) {
		return []byte("{}"), nil
	}
	return json.Marshal(h)
}

// --- unmarshal ----------------------------------------------------------

// ParseMessage walks a multipart ZMTP receive and returns the decoded
// Message. The HMAC is verified against key; mismatches return an
// error so callers don't silently process tampered traffic.
//
// frames is the slice as received from go-zeromq/zmq4 — first element
// may be a topic (PUB→SUB) or empty (DEALER→ROUTER).
func ParseMessage(frames [][]byte, key []byte) (*Message, error) {
	// Find the delimiter; everything before it is identities, everything
	// after is the signed payload.
	delim := -1
	for i, f := range frames {
		if bytes.Equal(f, []byte(delimiter)) {
			delim = i
			break
		}
	}
	if delim < 0 {
		return nil, errors.New("jupyter: <IDS|MSG> delimiter not found")
	}
	if len(frames) < delim+6 {
		return nil, fmt.Errorf("jupyter: short message after delimiter (got %d frames)", len(frames))
	}

	identities := frames[:delim]
	sigFrame := frames[delim+1]
	headerJSON := frames[delim+2]
	parentJSON := frames[delim+3]
	metaJSON := frames[delim+4]
	contentJSON := frames[delim+5]
	var buffers [][]byte
	if len(frames) > delim+6 {
		buffers = frames[delim+6:]
	}

	if len(key) > 0 {
		want := sign(key, headerJSON, parentJSON, metaJSON, contentJSON)
		if !hmac.Equal([]byte(want), sigFrame) {
			return nil, errors.New("jupyter: HMAC signature mismatch")
		}
	}

	m := &Message{
		Identities: cloneFrames(identities),
		Metadata:   append([]byte(nil), metaJSON...),
		Content:    append([]byte(nil), contentJSON...),
		Buffers:    cloneFrames(buffers),
	}
	if err := json.Unmarshal(headerJSON, &m.Header); err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	// parent_header may legitimately be the empty object {}; tolerate it.
	if len(parentJSON) > 0 && !bytes.Equal(parentJSON, []byte("{}")) {
		if err := json.Unmarshal(parentJSON, &m.ParentHeader); err != nil {
			return nil, fmt.Errorf("decode parent_header: %w", err)
		}
	}
	return m, nil
}

func cloneFrames(in [][]byte) [][]byte {
	if len(in) == 0 {
		return nil
	}
	out := make([][]byte, len(in))
	for i, f := range in {
		out[i] = append([]byte(nil), f...)
	}
	return out
}

// --- HMAC ---------------------------------------------------------------

// sign computes hex(HMAC-SHA256(key, header||parent||metadata||content))
// over the four JSON parts. The concatenation buffer is taken from a
// pool so chatty iopub workloads don't churn the allocator.
func sign(key []byte, parts ...[]byte) string {
	if len(key) == 0 {
		return ""
	}
	bufp := hmacScratch.Get().(*[]byte)
	buf := (*bufp)[:0]
	for _, p := range parts {
		buf = append(buf, p...)
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(buf)
	out := mac.Sum(nil)

	*bufp = buf
	hmacScratch.Put(bufp)
	return hex.EncodeToString(out)
}

// --- common content shapes ----------------------------------------------

// ExecuteRequest is the content for an execute_request shell message.
type ExecuteRequest struct {
	Code            string         `json:"code"`
	Silent          bool           `json:"silent"`
	StoreHistory    bool           `json:"store_history"`
	UserExpressions map[string]any `json:"user_expressions"`
	AllowStdin      bool           `json:"allow_stdin"`
	StopOnError     bool           `json:"stop_on_error"`
}

// ExecuteReply is the content of the shell-side execute_reply. The
// kernel sets `status` to "ok", "error", or "abort"; on "error" the
// kernel also publishes an `error` message on iopub.
type ExecuteReply struct {
	Status         string   `json:"status"`
	ExecutionCount int      `json:"execution_count"`
	ENAME          string   `json:"ename,omitempty"`
	EVALUE         string   `json:"evalue,omitempty"`
	Traceback      []string `json:"traceback,omitempty"`
}

// StreamContent is published on iopub for stdout/stderr writes.
type StreamContent struct {
	Name string `json:"name"` // "stdout" or "stderr"
	Text string `json:"text"`
}

// ExecuteResultContent is published when the cell evaluates to a value
// (Out[N] in Jupyter notebooks).
type ExecuteResultContent struct {
	ExecutionCount int            `json:"execution_count"`
	Data           map[string]any `json:"data"`
	Metadata       map[string]any `json:"metadata"`
}

// DisplayDataContent is published by IPython.display.* calls.
type DisplayDataContent struct {
	Data     map[string]any `json:"data"`
	Metadata map[string]any `json:"metadata"`
}

// ErrorContent is published when execution raises.
type ErrorContent struct {
	ENAME     string   `json:"ename"`
	EVALUE    string   `json:"evalue"`
	Traceback []string `json:"traceback"`
}

// StatusContent is published before/after every execution: "busy" then
// "idle". The trailing "idle" with parent_header.msg_id == our request
// is our cue that all output is in.
type StatusContent struct {
	ExecutionState string `json:"execution_state"`
}
