package provisioner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/client"
)

// deploy_wrap.go — m2 sidecar layering for deployed images.
//
// After railpack produces an OCI image, we add ONE more layer:
//
//	FROM <railpack-image>
//	COPY authproxy /usr/local/bin/authproxy
//	ENV  AGENTRY_AUTHPROXY_EXEC=<original entrypoint+cmd, joined>
//	ENTRYPOINT ["/usr/local/bin/authproxy"]
//
// At runtime authproxy reads AGENTRY_AUTHPROXY_EXEC and spawns the
// user's app on PORT+1, listening itself on PORT. When the deployment
// env stamps AGENTRY_AUTH_ENABLED=true it serves the login/signup
// surface; otherwise it stays in passthrough mode and behaves like a
// transparent reverse proxy. So we can ALWAYS wrap — the runtime
// branching is in env, not in the image.
//
// This keeps every deployed image structurally identical regardless
// of whether the user has auth on; toggling auth means stamping env
// + redeploy, no image rebuild. ~20 MB layer cost — acceptable.

// authproxyBinaryOnDisk is where Dockerfile.provisioner installs the
// sidecar binary. Kept in sync with the COPY directive there.
const authproxyBinaryOnDisk = "/usr/local/share/agentry/authproxy"

// wrapImageWithAuthproxy takes a railpack-built image, layers
// authproxy on top, and re-tags the wrapped result so the original
// imageRef points at the wrapped image. deploy_run is unchanged — it
// still sees just one tag.
//
// Returns nil on success. Failures are surfaced verbatim; the caller
// turns them into a 502 with the wrap-step output appended.
func (p *Provisioner) wrapImageWithAuthproxy(ctx context.Context, cli *client.Client, imageRef string) error {
	if _, err := os.Stat(authproxyBinaryOnDisk); err != nil {
		// Older provisioner image without the bake step. Skip the wrap
		// rather than fail the build — the deployed image just won't
		// have auth-flow capability until the provisioner is updated.
		log.Printf("deploy-wrap: authproxy binary missing at %s; skipping wrap (auth flow won't be served)",
			authproxyBinaryOnDisk)
		return nil
	}

	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("inspect railpack image: %w", err)
	}
	origExec, err := joinDockerCmd(inspect.Config.Entrypoint, inspect.Config.Cmd)
	if err != nil {
		return fmt.Errorf("extract original entrypoint: %w", err)
	}
	log.Printf("deploy-wrap: image=%s original-exec=%q — wrapping with authproxy", imageRef, origExec)

	tarBytes, err := buildWrapContextTar(imageRef, origExec)
	if err != nil {
		return fmt.Errorf("build wrap context: %w", err)
	}

	// ImageBuild reads the Dockerfile + COPY sources from the tar
	// stream; we set the Tags so the wrapped result steals the
	// original ref (rather than orphaning the unwrapped one — we
	// don't need it).
	resp, err := cli.ImageBuild(ctx, bytes.NewReader(tarBytes), buildOptionsForWrap(imageRef))
	if err != nil {
		return fmt.Errorf("ImageBuild start: %w", err)
	}
	defer resp.Body.Close()
	// Drain + scan for errors. Docker streams a JSON line per step;
	// any line with {"error":"..."} means the build failed.
	if err := drainBuildResponse(resp.Body); err != nil {
		return fmt.Errorf("ImageBuild stream: %w", err)
	}

	// Re-inspect: confirm the new image carries authproxy as the
	// entrypoint. Catches the corner case where buildkit returns 0
	// on a partial build (same defensive check as the railpack step).
	post, _, err := cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("post-wrap inspect: %w", err)
	}
	if len(post.Config.Entrypoint) == 0 || !strings.HasSuffix(post.Config.Entrypoint[0], "authproxy") {
		return errors.New("post-wrap inspect: entrypoint is not authproxy — wrap silently failed")
	}
	return nil
}

// buildOptionsForWrap returns docker SDK ImageBuildOptions sized for a
// tiny single-step layer. The defaults BUILDKIT-or-not-BUILDKIT will
// pick the daemon's preferred builder; we don't need any of the
// BuildKit features for one COPY + one ENV + one ENTRYPOINT.
func buildOptionsForWrap(imageRef string) (opts buildOpts) {
	opts = buildOpts{
		Tags:           []string{imageRef},
		Dockerfile:     "Dockerfile",
		Remove:         true,
		ForceRemove:    true,
		SuppressOutput: false,
		PullParent:     false, // base image is local — never re-pull
	}
	return
}

// buildOpts is an alias so callers don't need to import the docker
// SDK to construct the build options. We only set the fields we care
// about; the rest stay zero (matching SDK defaults). Defined as a
// type for clarity even though it's structurally the SDK's
// ImageBuildOptions.
type buildOpts = build.ImageBuildOptions

// buildWrapContextTar produces an in-memory tar containing the
// Dockerfile + the authproxy binary. Tar instead of a directory so we
// don't need a temp dir on disk (the docker SDK ImageBuild wants a
// reader anyway). About 20 MB total.
func buildWrapContextTar(baseImageRef, origExec string) ([]byte, error) {
	dockerfile := renderWrapDockerfile(baseImageRef, origExec)

	binData, err := os.ReadFile(authproxyBinaryOnDisk)
	if err != nil {
		return nil, fmt.Errorf("read authproxy binary: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{"Dockerfile", 0o644, []byte(dockerfile)},
		{"authproxy", 0o755, binData},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name,
			Mode: f.mode,
			Size: int64(len(f.data)),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderWrapDockerfile produces the tiny single-layer Dockerfile.
//
// We escape the entrypoint string by using JSON encoding — the
// Dockerfile parser is fine with %q-style quoting for ENV values, and
// json.Marshal handles every edge case we care about (embedded
// quotes, newlines, unicode).
func renderWrapDockerfile(baseImageRef, origExec string) string {
	execEsc, _ := json.Marshal(origExec) // never errors on string
	return fmt.Sprintf(`FROM %s
COPY authproxy /usr/local/bin/authproxy
RUN chmod +x /usr/local/bin/authproxy
ENV AGENTRY_AUTHPROXY_EXEC=%s
ENTRYPOINT ["/usr/local/bin/authproxy"]
CMD []
`, baseImageRef, string(execEsc))
}

// joinDockerCmd reconstructs a runnable command from an image's
// Config.Entrypoint + Config.Cmd. Docker's run rule is:
//
//   - If Entrypoint is set, it runs as the command; Cmd is appended
//     as default args.
//   - If Entrypoint is empty, Cmd is the command.
//
// We honor both. If both are empty, refuse with an error so the
// caller fails the build cleanly instead of shipping an image
// authproxy can't run.
func joinDockerCmd(entrypoint, cmd []string) (string, error) {
	parts := append([]string{}, entrypoint...)
	parts = append(parts, cmd...)
	if len(parts) == 0 {
		return "", errors.New("base image has no ENTRYPOINT and no CMD; nothing to wrap")
	}
	return joinShellArgs(parts), nil
}

// joinShellArgs joins argv into a single string the sidecar's
// splitArgv can re-parse. Args with spaces get double-quoted;
// embedded double-quotes are stripped (consistent with the producer
// side — see pkg/handlers/project_authproxy.go::joinAuthproxyExec).
func joinShellArgs(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		a = strings.ReplaceAll(a, `"`, ``)
		if strings.Contains(a, " ") {
			a = `"` + a + `"`
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// drainBuildResponse reads the streamed JSON lines from the daemon
// and surfaces any {"error":"..."} as a Go error. Empty body / EOF
// without an error line is treated as success.
func drainBuildResponse(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var line struct {
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode build line: %w", err)
		}
		if line.Error != "" {
			return errors.New(line.Error)
		}
		if line.ErrorDetail.Message != "" {
			return errors.New(line.ErrorDetail.Message)
		}
		// We could log line.Stream here for visibility; the existing
		// railpack path doesn't stream-log either, so we stay quiet to
		// keep deploy logs focused on the user's build output.
	}
}
