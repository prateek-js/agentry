package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
)

// defaultPullExcludes are the GNU-tar --exclude patterns we send by
// default. The list is conservative — every entry is either
// reproducible from a checked-in lockfile / config (node_modules from
// package.json, .venv from requirements.txt, dist/.next from
// `npm run build`) OR a per-user editor cache (.idea, .vscode is
// kept by default because users sometimes want their workspace
// settings, .idea isn't kept because it leaks JetBrains state). Each
// pattern matches anywhere in the tree — `node_modules` skips both
// `/workspace/projects/app/node_modules/` and a nested one.
func defaultPullExcludes() []string {
	return []string{
		"node_modules",
		".next",
		".nuxt",
		".svelte-kit",
		".turbo",
		"dist",
		"build",
		".cache",
		".parcel-cache",
		"__pycache__",
		".pytest_cache",
		".venv",
		"venv",
		"target",
		".gradle",
		".idea",
	}
}

// stringSliceFlag is the standard repeatable-flag adapter — `-exclude
// X -exclude Y` accumulates into a slice. Used by pull's --exclude.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// progressReader counts bytes flowing through Read() and re-paints a
// "downloaded N MB" line to stderr at most every 500 ms. No goroutine,
// no Stop(): when the upstream returns io.EOF we print a final \n and
// move on. Useful when wrapping a tunnel-backed http response body
// being fed into an extractor or io.Copy; the user gets continuous
// feedback without spamming the terminal on every Read.
type progressReader struct {
	r         io.Reader
	label     string
	total     int64
	lastPrint time.Time
	done      bool
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.total += int64(n)
	if time.Since(p.lastPrint) > 500*time.Millisecond {
		fmt.Fprintf(os.Stderr, "\r%s %s", p.label, humanBytes(p.total))
		p.lastPrint = time.Now()
	}
	if err == io.EOF && !p.done {
		fmt.Fprintf(os.Stderr, "\r%s %s\n", p.label, humanBytes(p.total))
		p.done = true
	}
	return n, err
}

// cmdPull copies the sandbox workspace to the user's laptop.
//
//	agentry pull                          pinned sandbox → ./<sandbox>/
//	agentry pull <sandbox>                explicit sandbox → ./<sandbox>/
//	agentry pull --to <dir>               destination override
//	agentry pull --archive <file.tar.gz>  save tarball instead of extracting
//	agentry pull --force                  overwrite non-empty destination
//
// One sandbox is one project; the sandbox name IS the project name and
// /workspace IS the project root. There's no `<sandbox>:<project>`
// shape because there's no second project to disambiguate against.
//
// Mechanism: POST /v1/archive/create on the sandbox runtime to tar.gz
// /workspace into a tmp file, then GET /v1/file/download to stream it
// back. Extract locally with leading-component stripping so files land
// at the destination directly instead of under a "workspace/" subdir.
func cmdPull(args []string) int {
	fs := flag.NewFlagSet("agentry pull", flag.ContinueOnError)
	toDir := fs.String("to", "", "destination directory (default: ./<sandbox>)")
	archivePath := fs.String("archive", "", "save the tarball to this path instead of extracting")
	force := fs.Bool("force", false, "overwrite a non-empty destination directory")
	noExcludes := fs.Bool("no-excludes", false, "include node_modules, .next, dist, etc. (default: skip them)")
	extraExclude := stringSliceFlag{}
	fs.Var(&extraExclude, "exclude", "extra GNU-tar --exclude pattern (repeatable)")
	flagArgs, posArgs := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) > 1 {
		return die("agentry pull [<sandbox>] [--to <dir>] [--archive <file>] [--force] [--exclude PATTERN]…")
	}
	explicit := ""
	if len(posArgs) == 1 {
		explicit = posArgs[0]
	}
	sandbox := resolveSandbox(explicit)
	if sandbox == "" {
		return die("no sandbox — pass <sandbox> or run `agentry sandbox use <id>` first")
	}

	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry login` first)", err)
	}
	sess, err := openTunnel(cfg)
	if err != nil {
		return die("dial broker: %v", err)
	}
	defer sess.Close()
	rt := &clusterStampedRT{next: tunnel.NewRoundTripper(sess), cluster: cfg.Cluster}
	client := &http.Client{Transport: rt}

	// Server-side scratch path. /tmp resets on container restart, so a
	// leftover from a crashed pull eventually self-cleans without an
	// explicit retention story.
	tarOnSandbox := "/tmp/agentry-pull-" + randTokenHex(4) + ".tar.gz"
	const baseURL = "http://bridge.invalid"

	excludes := []string(extraExclude)
	if !*noExcludes {
		excludes = append(defaultPullExcludes(), excludes...)
	}

	// Pull is a two-phase operation that USED to print nothing for
	// minutes on big workspaces, so the user couldn't tell it from a
	// hung tunnel. Surface each phase to stderr.
	if len(excludes) > 0 {
		fmt.Fprintf(os.Stderr, "agentry: archiving /workspace inside %s (skipping %s)…\n",
			sandbox, strings.Join(excludes, ", "))
	} else {
		fmt.Fprintf(os.Stderr, "agentry: archiving /workspace inside %s (no excludes)…\n", sandbox)
	}
	if err := runtimeArchiveCreate(client, baseURL, sandbox, "/workspace", tarOnSandbox, excludes); err != nil {
		return die("archive create: %v", err)
	}

	fmt.Fprintf(os.Stderr, "agentry: downloading tarball…\n")
	rc, err := runtimeDownload(client, baseURL, sandbox, tarOnSandbox)
	if err != nil {
		return die("download: %v", err)
	}
	defer rc.Close()
	// Inline progress counter; Read() updates the byte total and
	// re-prints to stderr at most every 500 ms so a multi-GB pull
	// shows movement without spamming the terminal.
	progress := &progressReader{r: rc, label: "agentry: downloaded"}

	if *archivePath != "" {
		f, err := os.Create(*archivePath)
		if err != nil {
			return die("create %s: %v", *archivePath, err)
		}
		n, err := io.Copy(f, progress)
		_ = f.Close()
		if err != nil {
			return die("write %s: %v", *archivePath, err)
		}
		fmt.Printf("agentry: pulled %s (%s) → %s\n", sandbox, humanBytes(n), *archivePath)
		return 0
	}

	dest := *toDir
	if dest == "" {
		dest = "./" + sandbox
	}
	if !*force {
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			return die("destination %s is not empty; pass --force to overwrite", dest)
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return die("mkdir %s: %v", dest, err)
	}
	files, bytes, err := extractStripOne(progress, dest)
	if err != nil {
		return die("extract: %v", err)
	}
	fmt.Printf("agentry: pulled %s (%d file(s), %s) → %s\n",
		sandbox, files, humanBytes(bytes), dest)
	return 0
}

// runtimeArchiveCreate asks the sandbox runtime to tar.gz `path` into
// `output`, applying GNU-tar --exclude patterns to skip reproducible
// trees (node_modules, .next, dist, …). Returns when the tarball is
// on disk inside the sandbox. baseURL is the prefix the tunnel
// transport rewrites in production ("http://bridge.invalid"); tests
// pass an httptest URL instead.
func runtimeArchiveCreate(client *http.Client, baseURL, sandbox, path, output string, exclude []string) error {
	body, _ := json.Marshal(map[string]any{
		"files":   []string{path},
		"output":  output,
		"exclude": exclude,
	})
	url := baseURL + "/api/sandboxes/" + sandbox + "/runtime/v1/archive/create"
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// runtimeDownload streams a file out of the sandbox. Caller closes the
// returned ReadCloser. Non-2xx returns an error with the body included
// so the user sees, e.g., "file not found" instead of just "404".
func runtimeDownload(client *http.Client, baseURL, sandbox, path string) (io.ReadCloser, error) {
	url := baseURL + "/api/sandboxes/" + sandbox + "/runtime/v1/file/download?file=" + path
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status=%d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.Body, nil
}

// extractStripOne reads a gzipped tar from r and writes its entries
// under dest, dropping the first path segment of every entry. The
// runtime tars `/workspace`; without the strip, every file would land
// under <dest>/workspace/, which is not what the user asked for.
//
// Symlinks point at whatever the original target was — we don't follow
// or resolve, just recreate. Hard links degrade to regular files
// (tar's link-target name is also stripped, so the link target may
// have already been written).
func extractStripOne(r io.Reader, dest string) (files int, total int64, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, total, nil
		}
		if err != nil {
			return files, total, fmt.Errorf("tar read: %w", err)
		}
		rel := stripLeadingSegment(hdr.Name)
		if rel == "" || rel == "." {
			continue
		}
		// Reject "../" escapes — a malicious tar could try to write
		// outside dest. filepath.Clean catches "a/../b" style, then we
		// re-check the result starts under dest.
		target := filepath.Join(dest, filepath.FromSlash(rel))
		clean, _ := filepath.Abs(target)
		root, _ := filepath.Abs(dest)
		if !strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator)) {
			return files, total, fmt.Errorf("tar entry %q escapes %s", hdr.Name, dest)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, total, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, total, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return files, total, err
			}
			n, err := io.Copy(f, tr)
			_ = f.Close()
			if err != nil {
				return files, total, err
			}
			total += n
			files++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, total, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return files, total, err
			}
		default:
			// Block devices, fifos, etc. — skip silently. We're
			// pulling source code, not a system image.
		}
	}
}

// stripLeadingSegment drops everything up to and including the first
// "/" in p. "workspace/foo/bar" → "foo/bar"; "workspace" → "".
func stripLeadingSegment(p string) string {
	// tar entries can start with "./" — collapse it before splitting.
	p = strings.TrimPrefix(p, "./")
	if i := strings.Index(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return ""
}

// randTokenHex returns 2*n hex chars (n random bytes). Used for the
// server-side tarball filename so concurrent pulls don't collide.
func randTokenHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// humanBytes renders a byte count as KiB/MiB/GiB. Output is a small
// status line so we don't need locale-aware formatting.
func humanBytes(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n) / k
	if v < k {
		return fmt.Sprintf("%.1f KiB", v)
	}
	v /= k
	if v < k {
		return fmt.Sprintf("%.1f MiB", v)
	}
	v /= k
	return fmt.Sprintf("%.1f GiB", v)
}
