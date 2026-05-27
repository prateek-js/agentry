package handlers

import (
	"bufio"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/models"
)

// FileReadHandler reads file contents with optional line range.
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

	data, err := os.ReadFile(req.File)
	if err != nil {
		Error(w, http.StatusNotFound, fmt.Sprintf("cannot read file: %v", err))
		return
	}

	content := string(data)

	// Apply line range if specified.
	if req.StartLine != nil || req.EndLine != nil {
		lines := strings.Split(content, "\n")
		start := 0
		end := len(lines)
		if req.StartLine != nil && *req.StartLine > 0 {
			start = *req.StartLine - 1
		}
		if req.EndLine != nil && *req.EndLine > 0 && *req.EndLine < end {
			end = *req.EndLine
		}
		if start >= len(lines) {
			start = len(lines)
		}
		if end > len(lines) {
			end = len(lines)
		}
		content = strings.Join(lines[start:end], "\n")
	}

	Success(w, "file read", models.FileReadData{
		File:    req.File,
		Content: content,
	})
}

// FileWriteHandler writes or appends content to a file.
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

	// Ensure parent directory exists.
	dir := filepath.Dir(req.File)
	if err := os.MkdirAll(dir, 0755); err != nil {
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

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if req.Append != nil && *req.Append {
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}

	f, err := os.OpenFile(req.File, flag, 0644)
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

	Success(w, "file written", models.FileWriteData{
		File:         req.File,
		BytesWritten: n,
	})
}

// FileListHandler lists directory contents.
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
	})
}

// FileFindHandler finds files matching a glob pattern.
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

	var matches []string
	_ = filepath.WalkDir(req.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		matched, _ := filepath.Match(req.Glob, d.Name())
		if matched {
			matches = append(matches, path)
		}
		return nil
	})

	Success(w, "files found", models.FileFindData{
		Path:  req.Path,
		Files: matches,
	})
}

// FileSearchHandler searches for regex matches in a file.
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

	var matches []string
	var lineNumbers []int
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, line)
			lineNumbers = append(lineNumbers, lineNum)
		}
	}

	Success(w, "search complete", models.FileSearchData{
		File:        req.File,
		Matches:     matches,
		LineNumbers: lineNumbers,
	})
}

// FileReplaceHandler replaces all occurrences of a string in a file.
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

	newContent := strings.ReplaceAll(content, req.OldStr, req.NewStr)
	if err := os.WriteFile(req.File, []byte(newContent), 0644); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("write error: %v", err))
		return
	}

	Success(w, "replaced", models.FileReplaceData{
		File:          req.File,
		ReplacedCount: count,
	})
}
