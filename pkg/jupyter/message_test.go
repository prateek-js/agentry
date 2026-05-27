package jupyter

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	key := []byte("sekret-key")

	body, _ := json.Marshal(ExecuteRequest{
		Code:         "print('hi')",
		StoreHistory: true,
	})
	msg := &Message{
		Header: Header{
			MsgID:    "msg-1",
			Session:  "sess-1",
			Username: "tester",
			MsgType:  "execute_request",
		},
		Content: body,
	}
	parts, err := msg.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}

	// Layout: <IDS|MSG>, sig, header, parent, meta, content. 6 frames.
	if len(parts) != 6 {
		t.Fatalf("frames = %d; want 6", len(parts))
	}
	if string(parts[0]) != "<IDS|MSG>" {
		t.Errorf("frame[0] = %q; want delimiter", parts[0])
	}
	if len(parts[1]) != 64 { // sha256 hex = 64 chars
		t.Errorf("sig len = %d; want 64", len(parts[1]))
	}

	got, err := ParseMessage(parts, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.MsgType() != "execute_request" {
		t.Errorf("MsgType = %s", got.MsgType())
	}
	if got.Header.MsgID != "msg-1" {
		t.Errorf("MsgID = %s", got.Header.MsgID)
	}

	var er ExecuteRequest
	if err := got.DecodeContent(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "print('hi')" {
		t.Errorf("decoded code = %q", er.Code)
	}
}

func TestHMACVerifyRejectsTampering(t *testing.T) {
	key := []byte("sekret-key")
	msg := &Message{
		Header:  Header{MsgID: "m", MsgType: "kernel_info_request"},
		Content: []byte("{}"),
	}
	parts, err := msg.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the content frame and re-parse.
	parts[5] = []byte(`{"injected":true}`)
	_, err = ParseMessage(parts, key)
	if err == nil {
		t.Fatal("expected HMAC mismatch error")
	}
}

func TestParseMessageMissingDelimiter(t *testing.T) {
	frames := [][]byte{[]byte("no-delimiter-here"), []byte("garbage")}
	if _, err := ParseMessage(frames, []byte("k")); err == nil {
		t.Fatal("expected error when delimiter is absent")
	}
}

func TestParseMessageShortAfterDelimiter(t *testing.T) {
	frames := [][]byte{[]byte("<IDS|MSG>"), []byte("sig"), []byte("{}")}
	if _, err := ParseMessage(frames, nil); err == nil {
		t.Fatal("expected error when payload is too short")
	}
}

func TestParseMessageNoKeySkipsVerify(t *testing.T) {
	// When key is empty (e.g. probing), HMAC verification is skipped.
	key := []byte("real-key")
	msg := &Message{
		Header:  Header{MsgType: "kernel_info_request"},
		Content: []byte("{}"),
	}
	parts, _ := msg.Marshal(key)
	// Mangle the sig — should still parse when caller passes nil key.
	parts[1] = []byte("deadbeef")
	if _, err := ParseMessage(parts, nil); err != nil {
		t.Fatalf("unkeyed parse failed: %v", err)
	}
}

func TestParseMessageIdentitiesPreserved(t *testing.T) {
	// Simulate an iopub frame: [topic, <IDS|MSG>, sig, header, parent, meta, content]
	key := []byte("k")
	msg := &Message{
		Header:  Header{MsgID: "x", MsgType: "status"},
		Content: []byte(`{"execution_state":"idle"}`),
	}
	signed, _ := msg.Marshal(key)
	withTopic := append([][]byte{[]byte("status.kernel.abc")}, signed...)

	got, err := ParseMessage(withTopic, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Identities) != 1 || !bytes.Equal(got.Identities[0], []byte("status.kernel.abc")) {
		t.Errorf("identities = %q; want [status.kernel.abc]", got.Identities)
	}
}

func TestSignDeterministic(t *testing.T) {
	a := sign([]byte("k"), []byte("a"), []byte("b"), []byte("c"))
	b := sign([]byte("k"), []byte("a"), []byte("b"), []byte("c"))
	if a != b {
		t.Errorf("HMAC nondeterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("sig length = %d; want 64", len(a))
	}
}

func TestSignDifferentKeyDifferentResult(t *testing.T) {
	a := sign([]byte("k1"), []byte("a"))
	b := sign([]byte("k2"), []byte("a"))
	if a == b {
		t.Errorf("HMAC didn't depend on key")
	}
}

func TestConnectionFileRoundTrip(t *testing.T) {
	c, err := NewConnectionFile("python")
	if err != nil {
		t.Fatal(err)
	}
	if c.ShellPort == c.IOPubPort {
		t.Errorf("port collision between shell and iopub: %d", c.ShellPort)
	}
	if c.SignatureScheme != "hmac-sha256" {
		t.Errorf("scheme = %q", c.SignatureScheme)
	}
	if len(c.Key) != 64 {
		t.Errorf("key len = %d; want 64-hex", len(c.Key))
	}
	if got := c.Endpoint("shell"); got != "tcp://127.0.0.1:"+itoa(c.ShellPort) {
		t.Errorf("endpoint = %q", got)
	}

	path, err := c.WriteTo("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(path) }()

	// Re-read the file and confirm fields stuck.
	var parsed ConnectionFile
	if err := readJSONFile(path, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Key != c.Key || parsed.ShellPort != c.ShellPort {
		t.Errorf("round-trip mismatch: %+v vs %+v", parsed, c)
	}
}

// --- small helpers to avoid importing the world ------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func removeFile(path string) error { return os.Remove(path) }

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
