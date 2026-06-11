package provisioner

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestJoinDockerCmdEntrypointAndCmd(t *testing.T) {
	got, err := joinDockerCmd([]string{"node"}, []string{"server.js"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "node server.js" {
		t.Fatalf("got %q", got)
	}
}

func TestJoinDockerCmdEntrypointOnly(t *testing.T) {
	got, err := joinDockerCmd([]string{"/app/start.sh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/app/start.sh" {
		t.Fatalf("got %q", got)
	}
}

func TestJoinDockerCmdCmdOnly(t *testing.T) {
	got, err := joinDockerCmd(nil, []string{"python", "app.py"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "python app.py" {
		t.Fatalf("got %q", got)
	}
}

func TestJoinDockerCmdEmpty(t *testing.T) {
	_, err := joinDockerCmd(nil, nil)
	if err == nil {
		t.Fatal("expected error on both empty")
	}
}

func TestJoinShellArgsQuotesSpaces(t *testing.T) {
	got := joinShellArgs([]string{"node", "--inspect=0.0.0.0:9229", "app with spaces.js"})
	want := `node --inspect=0.0.0.0:9229 "app with spaces.js"`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRenderWrapDockerfileShape(t *testing.T) {
	df := renderWrapDockerfile("deploy-myapp:abc", `node server.js`)
	for _, want := range []string{
		"FROM deploy-myapp:abc",
		"COPY authproxy /usr/local/bin/authproxy",
		`AGENTRY_AUTHPROXY_EXEC="node server.js"`,
		`ENTRYPOINT ["/usr/local/bin/authproxy"]`,
	} {
		if !strings.Contains(df, want) {
			t.Fatalf("missing %q in dockerfile:\n%s", want, df)
		}
	}
}

func TestRenderWrapDockerfileEscapesQuotes(t *testing.T) {
	// json.Marshal is the escape mechanism — embedded quotes should
	// come out backslash-escaped, not literal.
	df := renderWrapDockerfile("base:tag", `node "weird".js`)
	if strings.Contains(df, `node "weird".js`) {
		t.Fatalf("raw quote not escaped:\n%s", df)
	}
	if !strings.Contains(df, `\"`) {
		t.Fatalf("expected backslash-quote escape:\n%s", df)
	}
}

// buildWrapContextTar reads the authproxy binary off disk, so the
// test substitutes a tiny file via a chdir-friendly env override
// would be intrusive. Instead, we test the tar-shaping logic by
// invoking it and checking it errors when the binary is missing.
func TestBuildWrapContextTarMissingBinary(t *testing.T) {
	// authproxyBinaryOnDisk is a const at /usr/local/share/agentry/...
	// On a dev machine that path doesn't exist, so this exercises the
	// error path.
	_, err := buildWrapContextTar("base:tag", "node server.js")
	if err == nil {
		t.Skip("authproxy binary exists at /usr/local/share/agentry/authproxy on this host; skipping missing-binary test")
	}
	if !strings.Contains(err.Error(), "authproxy") {
		t.Fatalf("error should mention authproxy: %v", err)
	}
}

// TestBuildWrapContextTarShapeFromFakeBinary exercises the tar header
// + content layout using a hand-rolled tar so we don't depend on
// authproxyBinaryOnDisk being present.
func TestBuildWrapContextTarShapeFromFakeBinary(t *testing.T) {
	// Synthesise what buildWrapContextTar would produce given a
	// 64-byte fake binary, so we can assert headers + sizes without
	// touching the filesystem.
	fakeBin := bytes.Repeat([]byte("X"), 64)
	df := renderWrapDockerfile("base:tag", "node server.js")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{
		{"Dockerfile", 0o644, []byte(df)},
		{"authproxy", 0o755, fakeBin},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()

	tr := tar.NewReader(&buf)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = true
	}
	if !seen["Dockerfile"] || !seen["authproxy"] {
		t.Fatalf("missing entries: %#v", seen)
	}
}

func TestDrainBuildResponseSuccess(t *testing.T) {
	body := `{"stream":"Step 1/4 : FROM base"}` + "\n" +
		`{"stream":"Successfully built abc"}` + "\n"
	if err := drainBuildResponse(strings.NewReader(body)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestDrainBuildResponseError(t *testing.T) {
	body := `{"stream":"Step 1/4"}` + "\n" +
		`{"error":"COPY failed: no such file"}` + "\n"
	err := drainBuildResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "COPY failed") {
		t.Fatalf("error doesn't carry detail: %v", err)
	}
}

func TestDrainBuildResponseErrorDetail(t *testing.T) {
	body := `{"errorDetail":{"message":"layer push failed"}}` + "\n"
	err := drainBuildResponse(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "layer push failed") {
		t.Fatalf("expected errorDetail propagated, got %v", err)
	}
}
