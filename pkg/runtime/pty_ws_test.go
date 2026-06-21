//go:build !windows
// +build !windows

package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentry-ai/agentry/pkg/handlers"
	"github.com/gorilla/websocket"
)

// dialPTY upgrades against the test server. The id is passed through as
// session_id so concurrent tests don't collide.
func dialPTY(t *testing.T, ts *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/v1/shell/pty?session_id=" + id + "&rows=24&cols=80"
	c, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readUntilFrame reads frames until it sees one with the requested
// type prefix, or fails the test on deadline.
func readUntilFrame(t *testing.T, c *websocket.Conn, want byte, deadline time.Duration) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(deadline))
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt != websocket.BinaryMessage || len(data) < 1 {
			continue
		}
		if data[0] == want {
			return data[1:]
		}
	}
}

// writeStdin sends an stdin frame.
func writeStdin(t *testing.T, c *websocket.Conn, payload string) {
	t.Helper()
	frame := append([]byte{handlers.FrameStdin}, []byte(payload)...)
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
}

func TestPTYEchoRoundTrip(t *testing.T) {
	ts := newTestServer(t, "")
	c := dialPTY(t, ts, "echo")

	// Give bash ~100ms to print its prompt before we write — otherwise
	// the stdin can land while bash is still initializing line buffering
	// and the echo is silently dropped.
	time.Sleep(150 * time.Millisecond)
	writeStdin(t, c, "echo round-trip-marker\n")

	deadline := time.Now().Add(3 * time.Second)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, data, err := c.ReadMessage()
		if err != nil {
			// Read deadlines are expected during quiet periods; keep
			// reading until the outer deadline fires.
			continue
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == handlers.FrameStdout {
			acc.Write(data[1:])
			if strings.Contains(acc.String(), "round-trip-marker") {
				return
			}
		}
	}
	t.Fatalf("never saw the echoed marker; saw=%q", acc.String())
}

func TestPTYResizeControlFrame(t *testing.T) {
	ts := newTestServer(t, "")
	c := dialPTY(t, ts, "resize")

	// Sending a resize must not break the connection.
	msg, _ := json.Marshal(map[string]any{
		"type": "resize", "rows": 40, "cols": 120,
	})
	if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatal(err)
	}

	// Send an stty command to verify the kernel saw it.
	writeStdin(t, c, "stty size\n")

	deadline := time.Now().Add(3 * time.Second)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == handlers.FrameStdout {
			acc.Write(data[1:])
			if strings.Contains(acc.String(), "40 120") {
				return
			}
		}
	}
	t.Fatalf("resize not applied; saw=%q", acc.String())
}

func TestPTYExclusiveClientLock(t *testing.T) {
	ts := newTestServer(t, "")
	c1 := dialPTY(t, ts, "exclusive")
	// Drain initial output so c1 has attached.
	c1.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, _ = c1.ReadMessage()

	// A second dial against the same session_id should succeed at the
	// WS level but be rejected at the attach step. The server closes
	// the WS quickly; the client observes a closed connection.
	url := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/v1/shell/pty?session_id=exclusive"
	c2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = c2.ReadMessage()
	if err == nil {
		t.Fatal("second client should not receive data while first is attached")
	}
}

func TestPTYReplayOnReconnect(t *testing.T) {
	ts := newTestServer(t, "")

	c1 := dialPTY(t, ts, "replay")
	// Quickly drain banner.
	c1.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		if _, _, err := c1.ReadMessage(); err != nil {
			break
		}
	}
	// Produce a distinctive marker.
	writeStdin(t, c1, "echo REPLAY-CANARY\n")

	// Read until we've seen the canary so we know the ring buffer has it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c1.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, data, err := c1.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == handlers.FrameStdout {
			if strings.Contains(string(data[1:]), "REPLAY-CANARY") {
				break
			}
		}
	}
	c1.Close()
	// Wait a beat for server-side detach.
	time.Sleep(200 * time.Millisecond)

	// Reattach. The replay snapshot should include the canary.
	c2 := dialPTY(t, ts, "replay")
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mt, data, err := c2.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == handlers.FrameStdout {
			if strings.Contains(string(data[1:]), "REPLAY-CANARY") {
				return
			}
		}
	}
}

func TestPTYExitFrame(t *testing.T) {
	ts := newTestServer(t, "")
	c := dialPTY(t, ts, "exit")
	// Drain banner.
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, _ = c.ReadMessage()

	writeStdin(t, c, "exit 7\n")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("connection closed before exit frame: %v", err)
		}
		if mt == websocket.BinaryMessage && len(data) >= 5 && data[0] == handlers.FrameExit {
			got := int32(binary.BigEndian.Uint32(data[1:5]))
			if got != 7 {
				t.Errorf("exit code = %d; want 7", got)
			}
			return
		}
	}
	t.Fatal("never saw exit frame")
}

func TestPTYListAndCloseHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	_ = dialPTY(t, ts, "listed")
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(ts.URL + "/v1/shell/ptys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Success bool `json:"success"`
		Data    struct {
			PTYs []struct {
				ID       string `json:"id"`
				Attached bool   `json:"attached"`
			} `json:"ptys"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range list.Data.PTYs {
		if p.ID == "listed" {
			found = true
			if !p.Attached {
				t.Errorf("listed pty should be attached")
			}
		}
	}
	if !found {
		t.Fatalf("pty 'listed' not in list: %+v", list.Data.PTYs)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/shell/pty/listed", nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Errorf("close status = %d", r2.StatusCode)
	}
}

func TestPTYUnknownControlIgnored(t *testing.T) {
	ts := newTestServer(t, "")
	c := dialPTY(t, ts, "weird")
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"who-knows"}`)); err != nil {
		t.Fatal(err)
	}
	// Connection should still be alive: send a ping-and-pong style poll.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	writeStdin(t, c, "echo still-alive\n")
	deadline, _ := ctx.Deadline()
	c.SetReadDeadline(deadline)
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("connection dropped: %v", err)
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == handlers.FrameStdout {
			if strings.Contains(string(data[1:]), "still-alive") {
				return
			}
		}
	}
}
