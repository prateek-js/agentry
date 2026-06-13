package handlers

import (
	"strings"
	"testing"
)

// project_scaffold_test.go — pin the contract that every "app-shaped"
// scaffold (streamlit, fastapi, static-html) honors $PORT at launch
// time. The authproxy sidecar shifts the spawned child's PORT to
// PORT+1 when AGENTRY_AUTH_ENABLED=true; a scaffold that hard-codes
// its port collides with the sidecar and dies with EADDRINUSE.

func TestScaffold_StreamlitHonorsPortEnv(t *testing.T) {
	s, err := buildProjectScaffold("x", "streamlit", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(s.config.StartCommand, " ")
	if !strings.Contains(cmd, "${PORT:-") {
		t.Fatalf("streamlit must use ${PORT:-…} substitution; got %q", cmd)
	}
	if !strings.Contains(cmd, "exec ") {
		t.Fatalf("streamlit must use `exec` so SIGTERM reaches the binary; got %q", cmd)
	}
}

func TestScaffold_FastAPIHonorsPortEnv(t *testing.T) {
	s, err := buildProjectScaffold("x", "fastapi", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(s.config.StartCommand, " ")
	if !strings.Contains(cmd, "${PORT:-") {
		t.Fatalf("fastapi must use ${PORT:-…} substitution; got %q", cmd)
	}
	if !strings.Contains(cmd, "exec ") {
		t.Fatalf("fastapi must use `exec`; got %q", cmd)
	}
}

func TestScaffold_StaticHTMLHonorsPortEnv(t *testing.T) {
	s, err := buildProjectScaffold("x", "static-html", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(s.config.StartCommand, " ")
	if !strings.Contains(cmd, "${PORT:-") {
		t.Fatalf("static-html must use ${PORT:-…} substitution; got %q", cmd)
	}
	if !strings.Contains(cmd, "exec ") {
		t.Fatalf("static-html must use `exec`; got %q", cmd)
	}
}

func TestScaffold_NextJSNoExplicitPort(t *testing.T) {
	// next dev honors PORT env natively — passing --port would
	// override that, which is exactly the collision the user hit.
	s, err := buildProjectScaffold("x", "nextjs", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(s.config.StartCommand, " ")
	if strings.Contains(cmd, "--port") || strings.Contains(cmd, " -p ") {
		t.Fatalf("nextjs scaffold must NOT pass --port (next dev honors PORT env); got %q", cmd)
	}
}

func TestScaffold_PythonScriptNeedsNoPort(t *testing.T) {
	// Sanity: python-script has no port concern. Confirms we didn't
	// over-correct and rewrite a kind that doesn't bind anything.
	s, err := buildProjectScaffold("x", "python-script", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(s.config.StartCommand, " ")
	if strings.Contains(cmd, "PORT") {
		t.Fatalf("python-script must not reference PORT; got %q", cmd)
	}
}
