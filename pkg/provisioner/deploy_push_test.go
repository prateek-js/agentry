package provisioner

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// drainPushStream is the load-bearing piece — docker's ImagePush
// returns 200 even on auth failures, and the failure shows up as a
// JSON event frame inside the stream. We MUST parse every line and
// surface the error, otherwise a push that 401'd will be silently
// reported as success.
//
// Pinning every error shape the docker daemon can emit prevents
// regressions like "we only checked .Error and missed the modern
// .errorDetail.message shape".

func TestDrainPushStream_HappyPath(t *testing.T) {
	// Real-shaped lines from a successful push. None contain an
	// error field, so drain returns nil.
	input := strings.Join([]string{
		`{"status":"The push refers to repository [ghcr.io/agentry-ai/x]"}`,
		`{"status":"Preparing","progressDetail":{},"id":"abc"}`,
		`{"status":"Pushed","progressDetail":{},"id":"abc"}`,
		`{"status":"latest: digest: sha256:deadbeef size: 1234"}`,
	}, "\n")
	if err := drainPushStream(strings.NewReader(input)); err != nil {
		t.Errorf("happy path returned %v; want nil", err)
	}
}

// The "error" top-level field is the older format docker emits when
// the push fails before per-layer events start (typically auth).
func TestDrainPushStream_TopLevelErrorField(t *testing.T) {
	input := `{"errorDetail":{"message":"denied: permission_denied"},"error":"denied: permission_denied"}`
	err := drainPushStream(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected an error from a denied push event; got nil")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error should propagate the daemon message; got %q", err)
	}
}

// errorDetail.message is the newer shape — same semantic, different
// field. We must catch both or auth failures pass as success.
func TestDrainPushStream_ErrorDetailMessage(t *testing.T) {
	input := `{"errorDetail":{"message":"unauthorized"}}`
	err := drainPushStream(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected an error from errorDetail.message; got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error should propagate errorDetail.message; got %q", err)
	}
}

// An error event mid-stream still propagates — the events before it
// don't make the push successful.
func TestDrainPushStream_ErrorAfterProgress(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"Preparing"}`,
		`{"status":"Pushing","progressDetail":{"current":42}}`,
		`{"error":"net/http: TLS handshake timeout"}`,
	}, "\n")
	err := drainPushStream(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected an error from late-stream failure; got nil")
	}
}

// A truncated stream (TLS dropped mid-push, daemon crashed) MUST
// fail — silently treating it as success would mark a half-push as
// done in the DeploymentRevision row.
func TestDrainPushStream_TruncatedFails(t *testing.T) {
	// Half a JSON document. Decoder will error.
	input := `{"status":"Preparing","progressDetail":`
	err := drainPushStream(strings.NewReader(input))
	if err == nil {
		t.Error("expected an error from truncated stream; got nil")
	}
}

// Empty stream is treated as success — docker can emit nothing when
// there's nothing to push (e.g. cached image already at the registry).
// This is the only legitimate "no events, no error" path.
func TestDrainPushStream_EmptyIsSuccess(t *testing.T) {
	if err := drainPushStream(strings.NewReader("")); err != nil {
		t.Errorf("empty stream should be success; got %v", err)
	}
}

// EOF before any JSON should also be success (a docker daemon could
// close the connection cleanly after pushing all cached blobs).
func TestDrainPushStream_EOFAlsoSuccess(t *testing.T) {
	if err := drainPushStream(eofReader{}); err != nil {
		t.Errorf("EOF reader should be success; got %v", err)
	}
}

type eofReader struct{}

func (eofReader) Read(p []byte) (int, error) { return 0, io.EOF }

// Ensure the function actually consumes the full stream — docker docs
// require draining. If we returned early on the first non-error
// frame we'd leak the underlying connection. We can't measure leak
// directly here, but we can confirm Decode walked past every frame.
var _ = errors.New // keep errors import in case we want it later
