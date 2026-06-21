package handlers

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/models"
	"github.com/bmatcuk/doublestar/v4"
)

// File-size guards. Picked so a single LLM call can't pull the whole
// node_modules tree across the tunnel by accident, but a real source
// file still round-trips cleanly.
const (
	defaultReadMaxBytes     = 1 << 20 // 1 MiB
	defaultListMaxResults   = 500
	defaultGrepMaxResults   = 200
	defaultGrepContextLines = 0
	binaryDetectBytes       = 8 << 10 // 8 KiB
)

// FileReadHandler reads a file. Output is streamed line-by-line so a
// range request (start_line / end_line) doesn't pay to load the whole
// file. Returns total_lines so the caller knows whether the slice
// covered the file or not.
func FileReadHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileReadRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.File == "" {
		Error(w, http.StatusBadRequest, "file is required")
		return
	}
	if isProtectedReadPath(req.File) {
		Error(w, http.StatusForbidden, "path is protected: "+credsMountPath+" is opaque to the runtime API")
		return
	}

	f, err := os.Open(req.File)
	if err != nil {
		Error(w, http.StatusNotFound, fmt.Sprintf("cannot read file: %v", err))
		return
	}
	defer f.Close()

	start := 1
	if req.StartLine != nil && *req.StartLine > 1 {
		start = *req.StartLine
	}
	end := 0 // 0 == unbounded
	if req.EndLine != nil && *req.EndLine > 0 {
		end = *req.EndLine
	}

	scanner := bufio.NewScanner(f)
	// Allow individual lines up to 1 MiB before bailing — handles
	// minified bundles without OOMing.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var out strings.Builder
	var bytesEmitted int
	var emittedStart, emittedEnd int
	truncated := false
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < start {
			continue
		}
		if end != 0 && lineNum > end {
			// Keep scanning to compute total_lines, but stop emitting.
			break
		}
		if bytesEmitted >= defaultReadMaxBytes {
			truncated = true
			// Keep counting lines for total_lines accuracy.
			continue
		}
		text := scanner.Text()
		if emittedStart == 0 {
			emittedStart = lineNum
		}
		emittedEnd = lineNum
		// Numbered format mirrors `cat -n`: 6-wide right-justified
		// line number, tab, content. Keeps long files compact while
		// giving the LLM something stable to refer to.
		if req.Format == "numbered" {
			fmt.Fprintf(&out, "%6d\t%s\n", lineNum, text)
		} else {
			out.WriteString(text)
			out.WriteByte('\n')
		}
		bytesEmitted += len(text) + 1
	}
	// Drain the rest to count total_lines, even if we stopped emitting.
	for scanner.Scan() {
		lineNum++
	}
	if err := scanner.Err(); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("scan error: %v", err))
		return
	}

	content := out.String()
	// Strip the trailing newline we added — the original file may or
	// may not have ended with one, and we don't want to imply one.
	if strings.HasSuffix(content, "\n") {
		content = content[:len(content)-1]
	}

	Success(w, "file read", models.FileReadData{
		File:       req.File,
		Content:    content,
		TotalLines: lineNum,
		StartLine:  emittedStart,
		EndLine:    emittedEnd,
		Truncated:  truncated,
	})
}

// FileWriteHandler writes or appends content to a file. Atomic on
// overwrite (temp file + rename) so a crash mid-write doesn't leave
// a half-written source file the LLM then tries to read back.
func FileWriteHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileWriteRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.File == "" {
		Error(w, http.StatusBadRequest, "file is required")
		return
	}

	dir := filepath.Dir(req.File)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("cannot create directory: %v", err))
		return
	}

	content := req.Content
	if req.LeadingNewline != nil && *req.LeadingNewline {
		content = "\n" + content
	}
	if req.TrailingNewline != nil && *req.TrailingNewline {
		content = content + "\n"
	}

	// Append mode: direct open, no atomicity needed (we're adding to
	// the tail). Overwrite mode: write to tmp, rename over.
	if req.Append != nil && *req.Append {
		f, err := os.OpenFile(req.File, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			Error(w, http.StatusInternalServerError, fmt.Sprintf("cannot open file: %v", err))
			return
		}
		defer f.Close()
		n, err := f.WriteString(content)
		if err != nil {
			Error(w, http.StatusInternalServerError, fmt.Sprintf("write error: %v", err))
			return
		}
		Success(w, "file written", models.FileWriteData{File: req.File, BytesWritten: n})
		return
	}

	n, err := atomicWriteFile(req.File, []byte(content), 0o644)
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("write error: %v", err))
		return
	}
	Success(w, "file written", models.FileWriteData{File: req.File, BytesWritten: n})
}

// atomicWriteFile writes data to a temp file in the same directory and
// renames it over the target. Same-directory tempfile is required for
// rename to be atomic on the same filesystem.
func atomicWriteFile(path string, data []byte, perm os.FileMode) (int, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	// On any error after CreateTemp, clean up the temp file.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	n, err := tmp.Write(data)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}
	cleanup = false
	return n, nil
}

// FileListHandler lists directory contents up to MaxDepth. Caps the
// result count so a sweep of /workspace doesn't dump 10k entries.
func FileListHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileListRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" {
		req.Path = "."
	}

	maxDepth := 2
	if req.MaxDepth != nil {
		maxDepth = *req.MaxDepth
	}
	showHidden := false
	if req.ShowHidden != nil {
		showHidden = *req.ShowHidden
	}
	includeSize := false
	if req.IncludeSize != nil {
		includeSize = *req.IncludeSize
	}
	includePerms := false
	if req.IncludePermissions != nil {
		includePerms = *req.IncludePermissions
	}

	var files []models.FileInfo
	dirCount, fileCount := 0, 0
	truncated := false

	_ = filepath.WalkDir(req.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(req.Path, path)
		if rel == "." {
			return nil
		}

		depth := strings.Count(rel, string(os.PathSeparator))
		if depth >= maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !showHidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if len(files) >= defaultListMaxResults {
			truncated = true
			return filepath.SkipAll
		}

		fi := models.FileInfo{
			Name:        d.Name(),
			Path:        path,
			IsDirectory: d.IsDir(),
		}

		if info, err := d.Info(); err == nil {
			if includeSize {
				size := info.Size()
				fi.Size = &size
			}
			if includePerms {
				perm := info.Mode().String()
				fi.Permissions = &perm
			}
			mt := info.ModTime().Format(time.RFC3339)
			fi.ModifiedTime = &mt
		}

		if !d.IsDir() {
			ext := filepath.Ext(d.Name())
			if ext != "" {
				fi.Extension = &ext
			}
			fileCount++
		} else {
			dirCount++
		}

		files = append(files, fi)
		return nil
	})

	if req.SortBy != nil {
		desc := req.SortDesc != nil && *req.SortDesc
		sort.Slice(files, func(i, j int) bool {
			switch *req.SortBy {
			case "name":
				if desc {
					return files[i].Name > files[j].Name
				}
				return files[i].Name < files[j].Name
			case "size":
				si, sj := int64(0), int64(0)
				if files[i].Size != nil {
					si = *files[i].Size
				}
				if files[j].Size != nil {
					sj = *files[j].Size
				}
				if desc {
					return si > sj
				}
				return si < sj
			default:
				return files[i].Name < files[j].Name
			}
		})
	}

	Success(w, "directory listed", models.FileListData{
		Path:           req.Path,
		Files:          files,
		TotalCount:     len(files),
		DirectoryCount: dirCount,
		FileCount:      fileCount,
		Truncated:      truncated,
	})
}

// FileFindHandler finds files matching a glob pattern. Uses
// doublestar so "**/*.py" and brace alternation ("{ts,tsx}") work as
// the LLM expects. Matches against the path relative to req.Path,
// NOT the basename — the previous version compared the glob against
// d.Name() only, which silently returned nothing for any pattern
// containing "/".
func FileFindHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileFindRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" || req.Glob == "" {
		Error(w, http.StatusBadRequest, "path and glob are required")
		return
	}

	pattern := req.Glob
	// Validate up-front so a bad pattern reports as bad-request, not
	// silent zero-match.
	if !doublestar.ValidatePattern(pattern) {
		Error(w, http.StatusBadRequest, "invalid glob pattern")
		return
	}

	var matches []string
	truncated := false
	_ = filepath.WalkDir(req.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= defaultListMaxResults {
			truncated = true
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(req.Path, path)
		if relErr != nil || rel == "." {
			return nil
		}
		ok, _ := doublestar.PathMatch(pattern, filepath.ToSlash(rel))
		if ok {
			matches = append(matches, path)
		}
		return nil
	})

	Success(w, "files found", models.FileFindData{
		Path:      req.Path,
		Files:     matches,
		Truncated: truncated,
	})
}

// FileSearchHandler searches for regex matches in a single file. Now
// returns FileGrepMatch records (file + line + text) so the response
// shape lines up with FileGrepHandler; the legacy parallel
// LineNumbers slice is filled too for backward compatibility.
func FileSearchHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileSearchRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.File == "" || req.Regex == "" {
		Error(w, http.StatusBadRequest, "file and regex are required")
		return
	}
	if isProtectedReadPath(req.File) {
		Error(w, http.StatusForbidden, "path is protected: "+credsMountPath+" is opaque to the runtime API")
		return
	}

	re, err := regexp.Compile(req.Regex)
	if err != nil {
		Error(w, http.StatusBadRequest, fmt.Sprintf("invalid regex: %v", err))
		return
	}

	f, err := os.Open(req.File)
	if err != nil {
		Error(w, http.StatusNotFound, fmt.Sprintf("cannot open file: %v", err))
		return
	}
	defer f.Close()

	var matches []models.FileGrepMatch
	var lineNumbers []int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, models.FileGrepMatch{
				File: req.File, Line: lineNum, Text: line,
			})
			lineNumbers = append(lineNumbers, lineNum)
		}
	}

	Success(w, "search complete", models.FileSearchData{
		File:        req.File,
		Matches:     matches,
		LineNumbers: lineNumbers,
	})
}

// FileGrepHandler is multi-file regex search. Walks Path, optionally
// filters by Glob (path-relative, doublestar), skips binaries, and
// returns structured matches with optional context lines.
func FileGrepHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileGrepRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" || req.Regex == "" {
		Error(w, http.StatusBadRequest, "path and regex are required")
		return
	}

	re, err := regexp.Compile(req.Regex)
	if err != nil {
		Error(w, http.StatusBadRequest, fmt.Sprintf("invalid regex: %v", err))
		return
	}

	if req.Glob != "" && !doublestar.ValidatePattern(req.Glob) {
		Error(w, http.StatusBadRequest, "invalid glob pattern")
		return
	}

	max := defaultGrepMaxResults
	if req.MaxResults != nil && *req.MaxResults > 0 {
		max = *req.MaxResults
	}
	ctxBefore := defaultGrepContextLines
	if req.ContextBefore != nil && *req.ContextBefore > 0 {
		ctxBefore = *req.ContextBefore
	}
	ctxAfter := defaultGrepContextLines
	if req.ContextAfter != nil && *req.ContextAfter > 0 {
		ctxAfter = *req.ContextAfter
	}

	var matches []models.FileGrepMatch
	total := 0
	truncated := false

	_ = filepath.WalkDir(req.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isProtectedReadPath(path) {
			return nil
		}
		if req.Glob != "" {
			rel, relErr := filepath.Rel(req.Path, path)
			if relErr != nil {
				return nil
			}
			ok, _ := doublestar.PathMatch(req.Glob, filepath.ToSlash(rel))
			if !ok {
				return nil
			}
		}
		got, fileTotal, err := grepFile(path, re, ctxBefore, ctxAfter, max-len(matches))
		if err != nil {
			return nil
		}
		total += fileTotal
		matches = append(matches, got...)
		if len(matches) >= max {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})

	Success(w, "grep complete", models.FileGrepData{
		Path:       req.Path,
		Matches:    matches,
		TotalFound: total,
		Truncated:  truncated,
	})
}

// grepFile scans one file. Returns up to capRemaining matches (with
// context) and the file's total match count (uncapped) so the caller
// can report "found 1200, returned 200".
func grepFile(path string, re *regexp.Regexp, ctxBefore, ctxAfter, capRemaining int) ([]models.FileGrepMatch, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	// Binary sniff: read a chunk, check for null bytes, then re-open
	// for the scan. Simpler than rewinding and avoids surprises with
	// buffered scanners.
	head := make([]byte, binaryDetectBytes)
	n, _ := io.ReadFull(f, head)
	if isBinary(head[:n]) {
		return nil, 0, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var allLines []string
	if ctxBefore > 0 || ctxAfter > 0 {
		// Need lines around matches — hold the file in memory.
		// 1 MiB cap kept the read handler honest; do the same here.
		var size int
		for scanner.Scan() {
			text := scanner.Text()
			size += len(text) + 1
			if size > defaultReadMaxBytes {
				break
			}
			allLines = append(allLines, text)
		}
		var out []models.FileGrepMatch
		count := 0
		for i, line := range allLines {
			if !re.MatchString(line) {
				continue
			}
			count++
			if capRemaining > 0 && len(out) < capRemaining {
				m := models.FileGrepMatch{File: path, Line: i + 1, Text: line}
				if ctxBefore > 0 {
					lo := i - ctxBefore
					if lo < 0 {
						lo = 0
					}
					m.ContextBefore = append([]string(nil), allLines[lo:i]...)
				}
				if ctxAfter > 0 {
					hi := i + 1 + ctxAfter
					if hi > len(allLines) {
						hi = len(allLines)
					}
					m.ContextAfter = append([]string(nil), allLines[i+1:hi]...)
				}
				out = append(out, m)
			}
		}
		return out, count, nil
	}

	// Fast path: no context, stream line-by-line.
	var out []models.FileGrepMatch
	count := 0
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !re.MatchString(text) {
			continue
		}
		count++
		if capRemaining > 0 && len(out) < capRemaining {
			out = append(out, models.FileGrepMatch{File: path, Line: line, Text: text})
		}
	}
	return out, count, nil
}

// isBinary returns true if buf has any null byte — a crude but
// well-established heuristic (it's what grep uses).
func isBinary(buf []byte) bool {
	for _, b := range buf {
		if b == 0 {
			return true
		}
	}
	return false
}

// FileReplaceHandler replaces literal old_str with new_str. Strict by
// default: errors with the actual count when old_str appears more
// than once and neither replace_all nor a matching expected_matches
// was passed. Atomic write (temp + rename).
func FileReplaceHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileReplaceRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.File == "" || req.OldStr == "" {
		Error(w, http.StatusBadRequest, "file and old_str are required")
		return
	}
	if isProtectedReadPath(req.File) {
		Error(w, http.StatusForbidden, "path is protected: "+credsMountPath+" is opaque to the runtime API")
		return
	}

	data, err := os.ReadFile(req.File)
	if err != nil {
		Error(w, http.StatusNotFound, fmt.Sprintf("cannot read file: %v", err))
		return
	}

	content := string(data)
	count := strings.Count(content, req.OldStr)
	if count == 0 {
		Error(w, http.StatusNotFound, "old_str not found in file")
		return
	}

	replaceAll := req.ReplaceAll != nil && *req.ReplaceAll
	if req.ExpectedMatches != nil {
		if count != *req.ExpectedMatches {
			Error(w, http.StatusUnprocessableEntity, fmt.Sprintf("expected %d matches, found %d — pass replace_all:true or narrow old_str", *req.ExpectedMatches, count))
			return
		}
	} else if !replaceAll && count > 1 {
		Error(w, http.StatusUnprocessableEntity, fmt.Sprintf("old_str matches %d places; pass replace_all:true or add surrounding context to make it unique", count))
		return
	}

	var newContent string
	if replaceAll || (req.ExpectedMatches != nil && *req.ExpectedMatches > 1) {
		newContent = strings.ReplaceAll(content, req.OldStr, req.NewStr)
	} else {
		// Single match: ReplaceAll on a unique substring is equivalent.
		newContent = strings.ReplaceAll(content, req.OldStr, req.NewStr)
	}

	if _, err := atomicWriteFile(req.File, []byte(newContent), 0o644); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("write error: %v", err))
		return
	}

	Success(w, "replaced", models.FileReplaceData{
		File:          req.File,
		ReplacedCount: count,
	})
}

// FileMultiEditHandler applies several edits to one file atomically.
// Each step uses the same strict semantics as file_replace. If any
// step would fail, the file is left unchanged and the response
// reports every step's outcome so the LLM can fix in one pass.
func FileMultiEditHandler(w http.ResponseWriter, r *http.Request) {
	var req models.FileMultiEditRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.File == "" {
		Error(w, http.StatusBadRequest, "file is required")
		return
	}
	if len(req.Edits) == 0 {
		Error(w, http.StatusBadRequest, "at least one edit is required")
		return
	}
	if len(req.Edits) > 100 {
		Error(w, http.StatusBadRequest, "too many edits in one call (max 100)")
		return
	}
	if isProtectedReadPath(req.File) {
		Error(w, http.StatusForbidden, "path is protected: "+credsMountPath+" is opaque to the runtime API")
		return
	}

	data, err := os.ReadFile(req.File)
	if err != nil {
		Error(w, http.StatusNotFound, fmt.Sprintf("cannot read file: %v", err))
		return
	}
	content := string(data)
	results := make([]models.FileEditStepResult, 0, len(req.Edits))

	for i, edit := range req.Edits {
		if edit.OldStr == "" {
			Error(w, http.StatusBadRequest, fmt.Sprintf("edit %d: old_str is required", i))
			return
		}
		count := strings.Count(content, edit.OldStr)
		if count == 0 {
			Error(w, http.StatusUnprocessableEntity, fmt.Sprintf("edit %d: old_str not found after prior edits", i))
			return
		}
		replaceAll := edit.ReplaceAll != nil && *edit.ReplaceAll
		if edit.ExpectedMatches != nil {
			if count != *edit.ExpectedMatches {
				Error(w, http.StatusUnprocessableEntity, fmt.Sprintf("edit %d: expected %d matches, found %d", i, *edit.ExpectedMatches, count))
				return
			}
		} else if !replaceAll && count > 1 {
			Error(w, http.StatusUnprocessableEntity, fmt.Sprintf("edit %d: old_str matches %d places; pass replace_all:true or narrow old_str", i, count))
			return
		}
		content = strings.ReplaceAll(content, edit.OldStr, edit.NewStr)
		results = append(results, models.FileEditStepResult{OldStr: edit.OldStr, ReplacedCount: count})
	}

	if _, err := atomicWriteFile(req.File, []byte(content), 0o644); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("write error: %v", err))
		return
	}

	Success(w, "multi-edit complete", models.FileMultiEditData{
		File:  req.File,
		Steps: results,
	})
}
