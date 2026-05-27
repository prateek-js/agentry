package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Download/upload size guardrails. Configurable via env at startup so
// operators can tune without recompiling.
var (
	// maxUploadSize caps a single upload. Defaults to 1 GiB.
	maxUploadSize = envInt64("SANDBOX_MAX_UPLOAD_BYTES", 1<<30)

	// uploadBufSize is the in-process copy buffer between the multipart
	// reader and the on-disk file. 256 KiB is a good compromise between
	// per-call allocation and syscall overhead.
	uploadBufSize = int(envInt64("SANDBOX_UPLOAD_BUF_BYTES", 256<<10))
)

// FileDownloadHandler streams a file with HTTP Range support.
//
// Usage:
//
//	GET /v1/file/download?file=/workspace/foo.bin
//	Range: bytes=0-1023        (optional)
//
// http.ServeContent handles If-Match / If-None-Match / Range / 206 / 416
// for us, including multi-range responses. We just have to open the file
// and tell it the modtime.
func FileDownloadHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("file")
	if path == "" {
		Error(w, http.StatusBadRequest, "file query parameter is required")
		return
	}
	if !filepath.IsAbs(path) {
		Error(w, http.StatusBadRequest, "file must be an absolute path")
		return
	}
	if isProtectedReadPath(path) {
		Error(w, http.StatusForbidden, "path is protected: "+credsMountPath+" is opaque to the runtime API")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			Error(w, http.StatusNotFound, "file not found")
			return
		}
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if info.IsDir() {
		Error(w, http.StatusBadRequest, "path is a directory")
		return
	}

	// Hint downstream that this is a bytestream. ServeContent will
	// override only if it can sniff a real content-type.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	// Filename hint for browsers / curl -OJ.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename=%q`, filepath.Base(path)))

	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// FileUploadHandler accepts multipart/form-data with a required "file"
// part and an optional "metadata" JSON part.
//
//	metadata: {
//	  "path":      "/workspace/dest.bin",   // required if no path param
//	  "mode":      "0644",                  // octal string, optional (default 0644)
//	  "overwrite": true                     // optional, default false
//	}
//
// As a convenience for curl-style callers, `path` and `overwrite` can
// also be passed as form fields or as `?path=…&overwrite=true` query
// params. Body precedence: metadata JSON > form field > query.
func FileUploadHandler(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader gives us a hard cap and a friendlier error than
	// an mid-stream EOF on oversized uploads.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	mr, err := r.MultipartReader()
	if err != nil {
		Error(w, http.StatusBadRequest, "multipart/form-data required: "+err.Error())
		return
	}

	meta := uploadMeta{
		Path:      r.URL.Query().Get("path"),
		Overwrite: r.URL.Query().Get("overwrite") == "true",
	}

	// Multipart parts must be streamed in order: each call to NextPart
	// closes the previous one, so we can't hold a reference to the file
	// part across iterations. We require metadata fields to precede the
	// "file" part, or that the path/overwrite come from query params.
	var wroteFile bool
	var n int64

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			Error(w, http.StatusBadRequest, "read part: "+err.Error())
			return
		}

		switch part.FormName() {
		case "metadata":
			body, err := io.ReadAll(io.LimitReader(part, 64<<10))
			_ = part.Close()
			if err != nil {
				Error(w, http.StatusBadRequest, "read metadata: "+err.Error())
				return
			}
			if uerr := json.Unmarshal(body, &meta); uerr != nil {
				Error(w, http.StatusBadRequest, "metadata is not valid JSON: "+uerr.Error())
				return
			}

		case "path":
			body, err := io.ReadAll(io.LimitReader(part, 4<<10))
			_ = part.Close()
			if err == nil && meta.Path == "" {
				meta.Path = string(body)
			}

		case "overwrite":
			body, _ := io.ReadAll(io.LimitReader(part, 8))
			_ = part.Close()
			if string(body) == "true" {
				meta.Overwrite = true
			}

		case "file":
			if wroteFile {
				_ = part.Close()
				Error(w, http.StatusBadRequest, `multiple "file" parts not allowed`)
				return
			}
			if meta.Path == "" {
				_ = part.Close()
				Error(w, http.StatusBadRequest,
					"destination path is required (set ?path=…, or place metadata before file in the multipart body)")
				return
			}
			if !filepath.IsAbs(meta.Path) {
				_ = part.Close()
				Error(w, http.StatusBadRequest, "path must be absolute")
				return
			}
			mode, perr := meta.parsedMode()
			if perr != nil {
				_ = part.Close()
				Error(w, http.StatusBadRequest, perr.Error())
				return
			}
			written, werr := streamUploadToDisk(part, meta.Path, meta.Overwrite, mode)
			_ = part.Close()
			if werr != nil {
				if errors.Is(werr, os.ErrExist) {
					Error(w, http.StatusConflict, "file exists; set overwrite=true to replace")
					return
				}
				if isMaxBytesErr(werr) {
					Error(w, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("upload exceeds %d bytes", maxUploadSize))
					return
				}
				Error(w, http.StatusInternalServerError, werr.Error())
				return
			}
			n = written
			wroteFile = true

		default:
			_ = part.Close()
		}
	}

	if !wroteFile {
		Error(w, http.StatusBadRequest, `multipart upload missing "file" part`)
		return
	}

	mode, _ := meta.parsedMode()
	Success(w, "uploaded", map[string]any{
		"path":          meta.Path,
		"bytes_written": n,
		"mode":          fmt.Sprintf("%#o", mode),
	})
}

// streamUploadToDisk copies src into path with the given mode. On any
// error the partial file is removed so we don't leave half-written
// state behind. Returns os.ErrExist when overwrite=false and the file
// already exists, so the caller can map it to 409.
func streamUploadToDisk(src io.Reader, path string, overwrite bool, mode os.FileMode) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir parent: %w", err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	dst, err := os.OpenFile(path, flags, mode)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, uploadBufSize)
	n, copyErr := io.CopyBuffer(dst, src, buf)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return n, closeErr
	}
	return n, nil
}

type uploadMeta struct {
	Path      string `json:"path"`
	Mode      string `json:"mode,omitempty"` // octal string, e.g. "0644"
	Overwrite bool   `json:"overwrite,omitempty"`
}

func (m *uploadMeta) parsedMode() (os.FileMode, error) {
	if m.Mode == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(m.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: must be octal (e.g. 0644)", m.Mode)
	}
	return os.FileMode(v) & os.ModePerm, nil
}

func isMaxBytesErr(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
