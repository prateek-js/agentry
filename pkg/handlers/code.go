package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/jupyter"
)

// CreateContextRequest is the body for POST /v1/code/contexts.
type CreateContextRequest struct {
	Language  string `json:"language"`
	ContextID string `json:"context_id,omitempty"` // optional; server generates one when empty
}

// CreateContextResponse is what the create handler returns.
type CreateContextResponse struct {
	ContextID string `json:"context_id"`
	Language  string `json:"language"`
	Status    string `json:"status"` // always "ready" on success
}

// ExecuteCodeRequest is the body for POST /v1/code/contexts/{id}/exec.
//
// Timeout caps the total wall-clock for the execute (kernel side). The
// HTTP response also closes when the client disconnects.
type ExecuteCodeRequest struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
}

// codeWriter is the SSE sink used by the exec handler. Wraps a
// ResponseWriter and the matching Flusher so each event hits the wire
// immediately.
type codeWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newCodeWriter(w http.ResponseWriter) (*codeWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("ResponseWriter does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	return &codeWriter{w: w, flusher: flusher}, nil
}

// event writes one SSE event with the given name and JSON payload.
func (c *codeWriter) event(name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	c.flusher.Flush()
	return nil
}

// CodeCreateContextHandler creates a new kernel.
func CodeCreateContextHandler(mgr *jupyter.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateContextRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Language == "" {
			req.Language = "python"
		}

		k, err := mgr.Spawn(r.Context(), req.ContextID, req.Language)
		if err != nil {
			switch {
			case errors.Is(err, jupyter.ErrKernelCapacity):
				Error(w, http.StatusServiceUnavailable, err.Error())
			case strings.Contains(err.Error(), "unknown language"):
				Error(w, http.StatusBadRequest, err.Error())
			case strings.Contains(err.Error(), "already in use"):
				Error(w, http.StatusConflict, err.Error())
			default:
				Error(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		Success(w, "context ready", CreateContextResponse{
			ContextID: k.ID(), Language: k.Language(), Status: "ready",
		})
	}
}

// CodeListContextsHandler enumerates running kernels.
func CodeListContextsHandler(mgr *jupyter.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		Success(w, "ok", map[string]any{"contexts": mgr.List()})
	}
}

// CodeDeleteContextHandler stops a kernel by id.
func CodeDeleteContextHandler(mgr *jupyter.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := mgr.Shutdown(id); err != nil {
			if errors.Is(err, jupyter.ErrKernelNotFound) {
				Error(w, http.StatusNotFound, "context not found")
				return
			}
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		Success(w, "shutdown", map[string]string{"context_id": id})
	}
}

// CodeInterruptHandler sends an interrupt to a kernel's running cell.
func CodeInterruptHandler(mgr *jupyter.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		k, err := mgr.Get(id)
		if err != nil {
			Error(w, http.StatusNotFound, "context not found")
			return
		}
		if err := k.Interrupt(r.Context()); err != nil {
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		Success(w, "interrupted", map[string]string{"context_id": id})
	}
}

// CodeExecuteHandler runs code in a context and streams output as SSE.
//
// Wire events:
//
//	event: status   data: {"state":"busy"|"idle"}
//	event: stream   data: {"name":"stdout|stderr","text":"..."}
//	event: result   data: {"data":{...},"metadata":{...}}
//	event: display  data: {"data":{...},"metadata":{...}}
//	event: error    data: {"ename":"...","evalue":"...","traceback":[...]}
//	event: reply    data: {"status":"ok|error|abort","execution_count":N,...}
//	event: done     data: {"dropped":N}
//
// The handler returns when (a) the kernel signals idle AND the shell
// reply arrived, (b) the client disconnects, or (c) the wall-clock
// timeout fires.
func CodeExecuteHandler(mgr *jupyter.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		k, err := mgr.Get(id)
		if err != nil {
			Error(w, http.StatusNotFound, "context not found")
			return
		}
		mgr.Touch(id)

		var req ExecuteCodeRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Code == "" {
			Error(w, http.StatusBadRequest, "code is required")
			return
		}

		sse, err := newCodeWriter(w)
		if err != nil {
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}

		ctx := r.Context()
		if req.TimeoutSeconds > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
			defer cancel()
		}

		stream, err := k.Execute(ctx, req.Code)
		if err != nil {
			_ = sse.event("error", map[string]any{
				"ename": "ExecuteFailed", "evalue": err.Error(),
			})
			return
		}
		defer func() {
			dropped := stream.Close()
			_ = sse.event("done", map[string]any{"dropped": dropped})
		}()

		streamSSE(ctx, sse, stream)
	}
}

// streamSSE is the inner loop: pump messages from iopub/shell to the
// client until both idle-on-our-msg-id and shell-reply have been seen,
// or ctx fires.
func streamSSE(ctx context.Context, sse *codeWriter, stream *jupyter.ExecuteStream) {
	var idle, gotReply bool
	for !idle || !gotReply {
		select {
		case <-ctx.Done():
			_ = sse.event("error", map[string]any{
				"ename": "ContextCancelled", "evalue": ctx.Err().Error(),
			})
			return
		case m, ok := <-stream.IOPub:
			if !ok {
				idle = true
				continue
			}
			switch m.MsgType() {
			case "stream":
				var c jupyter.StreamContent
				_ = m.DecodeContent(&c)
				_ = sse.event("stream", c)
			case "execute_result":
				var c jupyter.ExecuteResultContent
				_ = m.DecodeContent(&c)
				_ = sse.event("result", c)
			case "display_data":
				var c jupyter.DisplayDataContent
				_ = m.DecodeContent(&c)
				_ = sse.event("display", c)
			case "error":
				var c jupyter.ErrorContent
				_ = m.DecodeContent(&c)
				_ = sse.event("error", c)
			case "status":
				var c jupyter.StatusContent
				_ = m.DecodeContent(&c)
				_ = sse.event("status", map[string]any{"state": c.ExecutionState})
				if c.ExecutionState == "idle" && m.ParentMsgID() == stream.MsgID {
					idle = true
				}
			default:
				// Forward unknown iopub messages opaquely so future
				// kernel features still surface to clients.
				_ = sse.event(m.MsgType(), json.RawMessage(m.Content))
			}
		case m, ok := <-stream.Shell:
			if !ok {
				continue
			}
			var r jupyter.ExecuteReply
			_ = m.DecodeContent(&r)
			_ = sse.event("reply", r)
			gotReply = true
		}
	}
}
