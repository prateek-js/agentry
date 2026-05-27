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

type FileFindRequest struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

type FileSearchRequest struct {
	File  string `json:"file"`
	Regex string `json:"regex"`
}

type FileReplaceRequest struct {
	File   string `json:"file"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

type FileReadData struct {
	File    string `json:"file"`
	Content string `json:"content"`
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
	Path  string   `json:"path"`
	Files []string `json:"files"`
}

type FileSearchData struct {
	File        string   `json:"file"`
	Matches     []string `json:"matches"`
	LineNumbers []int    `json:"line_numbers"`
}

type FileReplaceData struct {
	File          string `json:"file"`
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

type PortInfo struct {
	Port        int    `json:"port"`
	PID         int    `json:"pid,omitempty"`
	ProcessName string `json:"process_name,omitempty"`
	State       string `json:"state"`
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

type ProjectCreateRequest struct {
	Name     string `json:"name"`
	Template string `json:"template,omitempty"` // "python-fastapi", "node-express", etc.
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
