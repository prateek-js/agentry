// Package models defines request/response types for the ad-sandbox runtime API.
package models

// Response wraps all API responses.
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

type ShellExecRequest struct {
	Command string   `json:"command"`
	ID      *string  `json:"id,omitempty"`
	ExecDir *string  `json:"exec_dir,omitempty"`
	Timeout *float64 `json:"timeout,omitempty"`
}

type ShellExecData struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
	Status    string `json:"status"` // "completed" | "hard_timeout" | "terminated"
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
}

// ---------------------------------------------------------------------------
// File Operations
// ---------------------------------------------------------------------------

type FileReadRequest struct {
	File      string `json:"file"`
	StartLine *int   `json:"start_line,omitempty"`
	EndLine   *int   `json:"end_line,omitempty"`
	// Format controls how lines are rendered:
	//   "" / "raw"      — plain content (default, back-compat)
	//   "numbered"      — cat -n style ("    1\tfoo"). The default the
	//                     MCP layer sets so the LLM can name lines
	//                     without inventing anchors.
	Format string `json:"format,omitempty"`
}

type FileWriteRequest struct {
	File            string `json:"file"`
	Content         string `json:"content"`
	Append          *bool  `json:"append,omitempty"`
	LeadingNewline  *bool  `json:"leading_newline,omitempty"`
	TrailingNewline *bool  `json:"trailing_newline,omitempty"`
}

type FileListRequest struct {
	Path               string  `json:"path"`
	Recursive          *bool   `json:"recursive,omitempty"`
	ShowHidden         *bool   `json:"show_hidden,omitempty"`
	MaxDepth           *int    `json:"max_depth,omitempty"`
	IncludeSize        *bool   `json:"include_size,omitempty"`
	IncludePermissions *bool   `json:"include_permissions,omitempty"`
	SortBy             *string `json:"sort_by,omitempty"`
	SortDesc           *bool   `json:"sort_desc,omitempty"`
}

// FileFindRequest globs by path-relative pattern (** + braces supported).
// The matcher operates on the relative path from Path, not the basename,
// so "**/*.py" finds Python files anywhere in the tree.
type FileFindRequest struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

type FileSearchRequest struct {
	File  string `json:"file"`
	Regex string `json:"regex"`
}

// FileReplaceRequest replaces literal OldStr with NewStr. Default
// semantics are strict: if OldStr appears more than once and neither
// ExpectedMatches nor ReplaceAll is set, the call errors with the
// actual count. Pass ReplaceAll:true for bulk renames; pass
// ExpectedMatches to assert a specific occurrence count.
type FileReplaceRequest struct {
	File            string `json:"file"`
	OldStr          string `json:"old_str"`
	NewStr          string `json:"new_str"`
	ExpectedMatches *int   `json:"expected_matches,omitempty"`
	ReplaceAll      *bool  `json:"replace_all,omitempty"`
}

// FileMultiEditRequest applies several edits to one file atomically.
// Each edit operates on the result of the previous; if any fails, the
// file is not written and the response details the failure.
type FileMultiEditRequest struct {
	File  string         `json:"file"`
	Edits []FileEditStep `json:"edits"`
}

type FileEditStep struct {
	OldStr          string `json:"old_str"`
	NewStr          string `json:"new_str"`
	ExpectedMatches *int   `json:"expected_matches,omitempty"`
	ReplaceAll      *bool  `json:"replace_all,omitempty"`
}

// FileGrepRequest is multi-file regex search. Path is walked; each
// text file (binaries skipped by null-byte sniff) is line-scanned and
// matches are returned with optional context lines.
type FileGrepRequest struct {
	Path          string `json:"path"`
	Regex         string `json:"regex"`
	Glob          string `json:"glob,omitempty"`
	MaxResults    *int   `json:"max_results,omitempty"`
	ContextBefore *int   `json:"context_before,omitempty"`
	ContextAfter  *int   `json:"context_after,omitempty"`
}

type FileReadData struct {
	File    string `json:"file"`
	Content string `json:"content"`
	// TotalLines is the file's full line count, so the LLM can tell
	// whether the slice it received covered the whole file.
	TotalLines int `json:"total_lines"`
	// StartLine / EndLine are the 1-based bounds actually returned
	// (echoes back the request after clamping).
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// Truncated is true when the response was capped (size limit hit).
	Truncated bool `json:"truncated,omitempty"`
}

type FileWriteData struct {
	File         string `json:"file"`
	BytesWritten int    `json:"bytes_written"`
}

type FileListData struct {
	Path           string     `json:"path"`
	Files          []FileInfo `json:"files"`
	TotalCount     int        `json:"total_count"`
	DirectoryCount int        `json:"directory_count"`
	FileCount      int        `json:"file_count"`
	Truncated      bool       `json:"truncated,omitempty"`
}

type FileInfo struct {
	Name         string  `json:"name"`
	Path         string  `json:"path"`
	IsDirectory  bool    `json:"is_directory"`
	Size         *int64  `json:"size,omitempty"`
	ModifiedTime *string `json:"modified_time,omitempty"`
	Permissions  *string `json:"permissions,omitempty"`
	Extension    *string `json:"extension,omitempty"`
}

type FileFindData struct {
	Path      string   `json:"path"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated,omitempty"`
}

// FileSearchData and FileGrepData both use FileGrepMatch so a single
// shape covers single-file (search) and multi-file (grep) results.
type FileSearchData struct {
	File    string           `json:"file"`
	Matches []FileGrepMatch  `json:"matches"`
	// LineNumbers retained for backward compatibility with existing
	// callers that pulled the parallel arrays out of the response.
	// Will be removed once internal callers migrate.
	LineNumbers []int `json:"line_numbers,omitempty"`
}

type FileGrepData struct {
	Path       string          `json:"path"`
	Matches    []FileGrepMatch `json:"matches"`
	TotalFound int             `json:"total_found"`
	Truncated  bool            `json:"truncated,omitempty"`
}

type FileGrepMatch struct {
	File          string   `json:"file"`
	Line          int      `json:"line"`
	Text          string   `json:"text"`
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

// FileReplaceData reports what changed plus the new total occurrence
// count of OldStr so the LLM can confirm intent.
type FileReplaceData struct {
	File          string `json:"file"`
	ReplacedCount int    `json:"replaced_count"`
}

type FileMultiEditData struct {
	File  string             `json:"file"`
	Steps []FileEditStepResult `json:"steps"`
}

type FileEditStepResult struct {
	OldStr        string `json:"old_str"`
	ReplacedCount int    `json:"replaced_count"`
}

// ---------------------------------------------------------------------------
// Process Management
// ---------------------------------------------------------------------------

type ProcessStopRequest struct {
	PID  *int    `json:"pid,omitempty"`
	Name *string `json:"name,omitempty"`
}

type ProcessInfo struct {
	PID     int     `json:"pid"`
	Name    string  `json:"name"`
	CPUPct  float64 `json:"cpu_pct"`
	MemMB   float64 `json:"mem_mb"`
	Command string  `json:"command"`
	Started string  `json:"started"`
}

// ---------------------------------------------------------------------------
// Port Management
// ---------------------------------------------------------------------------

type PortWaitRequest struct {
	Port           int  `json:"port"`
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
}

// PortsListData is the response shape for GET /v1/ports. Splits ports
// the project manager OWNS from "unmanaged" listeners — anything bound
// by a bare command_run / command_start. The unmanaged list is the
// signal the LLM sees the moment it falls off the project pattern:
// surfacing the anomaly in the response is faster than waiting for the
// user to redirect.
type PortsListData struct {
	Ports              []PortInfo `json:"ports"`
	UnmanagedListeners []PortInfo `json:"unmanaged_listeners,omitempty"`
}

type PortInfo struct {
	Port        int    `json:"port"`
	PID         int    `json:"pid,omitempty"`
	ProcessName string `json:"process_name,omitempty"`
	State       string `json:"state"`
	// Managed is true when this listener belongs to a registered
	// project's process group. False/absent means the process was
	// started with command_run / command_start and isn't covered by
	// the project manager — that's the LLM's cue to wire it into a
	// project.
	Managed bool `json:"managed,omitempty"`
	// Address is the literal bind host from the LISTEN socket
	// ("0.0.0.0", "127.0.0.1", "::", "::1", or a specific interface
	// IP). Used by the dashboard to distinguish ports that can be
	// reached from outside the sandbox from loopback-only sockets
	// (Jupyter kernels, internal IPC, etc.).
	Address string `json:"address,omitempty"`
	// Loopback is true when Address is on the loopback range. Computed
	// server-side so every consumer agrees on the classification
	// without having to ParseIP themselves.
	Loopback bool `json:"loopback,omitempty"`
}

// ---------------------------------------------------------------------------
// Project Manager (key differentiator)
// ---------------------------------------------------------------------------

// ProjectConfig is the .sandbox-project.json format.
//
// The project manager no longer pre-allocates ports. It runs your
// start_command and discovers any TCP ports the resulting process
// tree binds (matched by process-group id). That set is reported in
// ProjectStatus.Ports.
type ProjectConfig struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"` // "app" | "agent" | "service"
	StartCommand []string          `json:"start_command"`
	AutoRestart  bool              `json:"auto_restart"`
	DependsOn    []string          `json:"depends_on,omitempty"` // project names to start first
	Env          map[string]string `json:"env,omitempty"`        // inline env vars
	EnvFile      string            `json:"env_file,omitempty"`   // .env file path
	HealthCheck  *HealthCheck      `json:"health_check,omitempty"`
	Resources    *ResourceLimits   `json:"resources,omitempty"`
}

type HealthCheck struct {
	// Port the probe targets. Required when HealthCheck is set —
	// the manager doesn't guess which of the discovered ports is
	// "the health one" because a project may listen on many.
	Port     int    `json:"port"`
	Path     string `json:"path"`               // HTTP path, e.g. "/health"
	Interval int    `json:"interval,omitempty"` // seconds, default 10
	Timeout  int    `json:"timeout,omitempty"`  // seconds, default 3
	Retries  int    `json:"retries,omitempty"`  // failures before unhealthy, default 3
}

type ResourceLimits struct {
	MaxMemoryMB int `json:"max_memory_mb,omitempty"`
	MaxCPUPct   int `json:"max_cpu_percent,omitempty"`
}

type ProjectStartRequest struct {
	Name string `json:"name"`
}

type ProjectStopRequest struct {
	Name string `json:"name"`
}

// ProjectCreateRequest is POST /v1/project/create. The handler
// scaffolds `.sandbox-project.json` (and a minimal starter file for
// the kind) so the LLM can't be tempted to skip the project pattern
// and tell the user to run a server by hand.
//
// Kinds:
//
//	"nextjs"        — Next.js App Router; start_command "npm run dev"
//	"static-html"   — vanilla HTML/CSS/JS; "python3 -m http.server $PORT" inside the project dir
//	"streamlit"     — Python data app; "streamlit run app.py --server.port $PORT"
//	"fastapi"       — Python API; "uvicorn app:app --host 0.0.0.0 --port $PORT"
//	"python-script" — long-running Python process; "python3 main.py"
//	"custom"        — bring your own; StartCommand is required
type ProjectCreateRequest struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	StartCommand []string `json:"start_command,omitempty"`
	Port         int      `json:"port,omitempty"`
}

type ProjectCreateData struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Path         string   `json:"path"`
	StartCommand []string `json:"start_command"`
	NextStep     string   `json:"next_step"`
	FilesWritten []string `json:"files_written"`
}

type ProjectStatus struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"` // "running" | "stopped" | "failed" | "starting"
	// Ports is every TCP port currently bound by any process in the
	// project's process group. Discovered live on each status call —
	// no pre-allocated pool. nil/empty when the project isn't running
	// or hasn't bound anything yet.
	Ports        []int  `json:"ports,omitempty"`
	PID          int    `json:"pid,omitempty"`
	Uptime       string `json:"uptime,omitempty"`
	RestartCount int    `json:"restart_count"`
	Health       string `json:"health,omitempty"` // "healthy" | "unhealthy" | "unknown"
	LastError    string `json:"last_error,omitempty"`
}

// ---------------------------------------------------------------------------
// System Metrics
// ---------------------------------------------------------------------------

type CPUInfo struct {
	Cores  int     `json:"cores"`
	Load1m float64 `json:"load_1m"`
	Load5m float64 `json:"load_5m"`
}

type MemoryInfo struct {
	TotalMB     float64 `json:"total_mb"`
	UsedMB      float64 `json:"used_mb"`
	AvailableMB float64 `json:"available_mb"`
	PctUsed     float64 `json:"pct_used"`
}

type DiskInfo struct {
	TotalGB     float64 `json:"total_gb"`
	UsedGB      float64 `json:"used_gb"`
	AvailableGB float64 `json:"available_gb"`
	PctUsed     float64 `json:"pct_used"`
}

// ---------------------------------------------------------------------------
// Archive Operations
// ---------------------------------------------------------------------------

type ArchiveCreateRequest struct {
	Files  []string `json:"files"`
	Output string   `json:"output"`
	Format string   `json:"format"`

	// Exclude is a list of GNU-tar --exclude patterns applied before
	// any input file is walked. Used by `agentry pull` to skip
	// reproducible-from-lockfile trees (node_modules, .next, dist,
	// __pycache__, …) so a research workspace doesn't ship 8 GB of
	// junk over the tunnel.
	Exclude []string `json:"exclude,omitempty"`
}

type ArchiveExtractRequest struct {
	Archive     string `json:"archive"`
	Destination string `json:"destination"`
}

// ---------------------------------------------------------------------------
// Git Operations
// ---------------------------------------------------------------------------

type GitInitRequest struct {
	Path string `json:"path,omitempty"`
}

type GitCommitRequest struct {
	Message string `json:"message"`
	AddAll  *bool  `json:"add_all,omitempty"`
	Path    string `json:"path,omitempty"`
}

type GitCloneRequest struct {
	URL  string `json:"url"`
	Path string `json:"path,omitempty"`
}

type GitCheckoutRequest struct {
	Branch string `json:"branch"`
	Create *bool  `json:"create,omitempty"`
	Path   string `json:"path,omitempty"`
}

type GitPathRequest struct {
	Path string `json:"path,omitempty"`
}

// ---------------------------------------------------------------------------
// Sandbox Info
// ---------------------------------------------------------------------------

type SandboxInfo struct {
	ID      string `json:"id"`
	HomeDir string `json:"home_dir"`
	Version string `json:"version"`
}

// ---------------------------------------------------------------------------
// Workspace Status
// ---------------------------------------------------------------------------

type WorkspaceStatus struct {
	AppRunning      bool            `json:"app_running"`
	Projects        []ProjectStatus `json:"projects"`
	OutputFiles     []string        `json:"output_files"`
	ActiveProcesses int             `json:"active_processes"`
	WorkspaceFiles  int             `json:"workspace_files"`
	ListeningPorts  []PortInfo      `json:"listening_ports"`
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

type ActivityData struct {
	LastToolCall   string `json:"last_tool_call"`
	RunningProcs   int    `json:"running_processes"`
	ListeningPorts int    `json:"listening_ports"`
	WorkspaceFiles int    `json:"workspace_files"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
}
