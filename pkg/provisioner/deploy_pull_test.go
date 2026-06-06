package provisioner

import (
	"strings"
	"testing"
)

// drainPullStream mirrors drainPushStream's contract: docker's
// ImagePull returns 200 immediately and reports auth / 404 failures as
// JSON event frames inside the stream. Missing one of those frames
// causes a deploy to "succeed" without the image — ContainerCreate
// then fails with a less clear message a few lines later. Pin every
// error shape so the obvious docker-daemon refactor doesn't silently
// re-introduce that bug.

func TestDrainPullStream_HappyPath(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"Pulling from agentry-ai/x"}`,
		`{"status":"Pulling fs layer","progressDetail":{},"id":"abc"}`,
		`{"status":"Downloading","progressDetail":{"current":1,"total":2},"id":"abc"}`,
		`{"status":"Pull complete","progressDetail":{},"id":"abc"}`,
		`{"status":"Digest: sha256:deadbeef"}`,
	}, "\n")
	if err := drainPullStream(strings.NewReader(input)); err != nil {
		t.Errorf("happy path returned %v; want nil", err)
	}
}

// The "errorDetail.message" shape is the one modern docker emits for
// auth failures. Older daemons stuff the same text into the top-level
// "error" field — both must surface.
func TestDrainPullStream_ErrorDetailShape(t *testing.T) {
	input := `{"errorDetail":{"message":"unauthorized: HTTP Basic: Access denied"},"error":"unauthorized: HTTP Basic: Access denied"}`
	if err := drainPullStream(strings.NewReader(input)); err == nil {
		t.Fatal("expected an error from a denied pull event; got nil")
	}
}

func TestDrainPullStream_TopLevelErrorOnly(t *testing.T) {
	// Some daemons emit only the top-level "error" key.
	input := `{"error":"manifest unknown"}`
	if err := drainPullStream(strings.NewReader(input)); err == nil {
		t.Fatal("expected an error from a top-level error event; got nil")
	}
}

// A pull that ends with a normal status frame AFTER reporting an
// error must still report the error — the daemon will sometimes emit
// a benign trailing event before closing the connection.
func TestDrainPullStream_ErrorThenStatus(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"Pulling from agentry-ai/x"}`,
		`{"errorDetail":{"message":"unauthorized"},"error":"unauthorized"}`,
		`{"status":"Aborted"}`,
	}, "\n")
	if err := drainPullStream(strings.NewReader(input)); err == nil {
		t.Fatal("expected the auth error to win over the trailing status frame")
	}
}
