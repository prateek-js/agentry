package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register attaches every ad-sandbox tool to the MCP server.
//
// House conventions (the model sees these via list_tools):
//
//   - Container port 8080 is the runtime API. User services bind to other
//     ports. Operator-side tunneling exposes those ports to end users —
//     the runtime itself doesn't reverse-proxy anything.
//   - Three supervision tiers; pick the LOWEST that fits, but reach for
//     `project_start` for any server you'll touch more than once:
//       command_run    — blocking one-shot (installs, tests, git, curl)
//       command_start  — background, NO supervision — throwaway watchers only
//       project_start  — supervised: auto-restart, health, depends_on, port discovery
//     For multi-service apps (backend + frontend) call `project_start_all`
//     — `depends_on` on each project handles ordering. Manifest format and
//     worked examples in /etc/sandbox/docs/projects.md.
//   - Code interpreter: `code_exec` runs Python statefully. Pass a new
//     `context_id` to spawn a kernel, reuse the id to keep state. Use for
//     EXPLORATION (≤30 lines per call). Source files you want to KEEP
//     belong in /workspace/projects/<name>/ via `file_write`.
//   - Ports listened on by a project are auto-discovered from its process
//     group — no pool, just bind what you want; `project_list` reports it.
//   - Credentials: when the operator configured them, they live read-only
//     at /etc/sandbox/creds/. Layout is OPERATOR-DEFINED — discover with
//     `command_run "ls -la /etc/sandbox/creds/"`; never assume filenames.
//     Read JSON/cert files IN-CODE; `file_read` returns 403 under that path.
//   - Image builds: `build-image --tag X .` (a buildah wrapper that
//     cross-builds linux/amd64 and exposes /etc/sandbox/creds as a build
//     context named `creds`, so a Dockerfile can `COPY --from=creds . /etc/sandbox/creds/`).
func Register(server *mcp.Server, c *Client) {
	// — Lifecycle ───────────────────────────────────────────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "sandbox_create",
		Description: "Spin up a fresh isolated sandbox container. Returns `sandbox_url` (the runtime endpoint every other tool needs), `sandbox_id` (the ACTUAL allocated id — may differ from what you asked for; see below), `bindings` (services the operator has pre-wired into this sandbox — each entry lists the env var names you read at runtime), and `env` (NAMES of additional env vars / secrets already loaded in the sandbox; values are never returned). " +
			"FIRST-TOUCH CHECKLIST after a successful create: (1) READ BOTH the `bindings` AND `env` arrays from the RESPONSE before deciding any service or credential is missing. If a service is in `bindings`, your code reads the listed env vars verbatim — DO NOT bind it again, DO NOT spin up an in-process substitute. If a name like `JIRA_TOKEN` / `SLACK_BOT_TOKEN` / `STRIPE_SECRET_KEY` is in `env`, the value is already in process.env inside the sandbox — DO NOT ask the user to provide it, DO NOT prompt for it, just use it. When the user names a service by name (\"jira\", \"slack\", \"stripe\", \"openai\", …) ALWAYS scan both arrays for a matching token BEFORE saying it isn't configured. (2) `command_run \"cat /etc/sandbox/docs/README.md\"` to load the recipe router, then read the cheat-sheet matching what the user asked for (e.g. agent.md, app.md). " +
			"Pass any descriptive `sandbox_id` (e.g. \"ecommerce-store\"). If that name is already taken by an unrelated sandbox, the server auto-allocates a fresh suffixed name (e.g. \"ecommerce-store-7f2a\") so you DON'T overwrite the existing one. " +
			"ALWAYS use the `sandbox_id` from the RESPONSE for every follow-up tool call in this conversation — never the one you passed in. " +
			"Set `reuse_existing: true` only when you genuinely want to attach to an existing sandbox by name (rare; typical use case is a CLI `attach` flow, not an LLM creating a new project).",
	}, sandboxCreate(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "sandbox_list",
		Description: "STATUS DISPLAY ONLY — for telling the user what's running. NEVER use this to pick a sandbox to write into. Every sandbox listed here holds a DIFFERENT project; writing into it overwrites someone else's work. For a new task ALWAYS call sandbox_create (it auto-suffixes if the name is taken — you get a fresh sandbox, you do NOT collide with an existing one). " +
			"If sandbox_create fails, REPORT the failure to the user and STOP — do not fall back to listing and reusing a survivor; that's the bug, not the workaround. " +
			"With no `sandbox_id`: returns `{sandbox_id, status}` for each sandbox — URLs intentionally omitted so the LLM can't accidentally target someone else's project. " +
			"With `sandbox_id`: returns full details (including sandbox_url) — ONLY call with sandbox_id when the USER explicitly names that sandbox (\"open my-store again\", \"keep working in jira-dash\"); never to recover from a failed create.",
	}, sandboxList(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "sandbox_delete",
		Description: "Tear down a sandbox by id. Always call when the user is done — sandboxes hold real Docker resources.",
	}, sandboxDelete(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "agentry_auth_setup",
		Description: "Scaffolds production-ready user authentication (Better-Auth: email + password, real users table, signed HTTP-only session cookies) into the sandbox's current Next.js app. " +
			"Use this tool whenever the user asks to \"add login\", \"set up auth\", \"let people sign up\", \"I need users\", \"make this require sign-in\", \"user passwords\", \"log in\", \"log out\", \"who's signed in\", or any request for account-based authentication. " +
			"Two-phase, idempotent. First call (no `mode`) returns the detected framework, the available storage backends, and the user-facing question to relay; second call ({mode: \"none\" | \"sqlite\" | \"binding:postgres\" | \"binding:mongodb\"}) performs the deterministic scaffold and returns a summary of files written + commands_to_run + next_steps. Re-calling on an already-wired project returns the current config without re-scaffolding. " +
			"Do NOT hand-roll authentication (bcrypt, argon2, JWT, custom cookies, in-memory user tables, localStorage sessions) in place of calling this tool — Better-Auth is the only supported auth path on agentry. OAuth (Google/GitHub), magic-link, and passkeys are not in this release; for those, tell the user \"those land next release; I'll wire email + password via agentry_auth_setup for now.\"",
	}, authSetup(c))
	// — Catalog (bindable external services + skills) ─────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "service_list",
		Description: "List services the current cluster offers (postgres, redis, mysql, mongodb, s3, smtp, stripe, openai, anthropic, clickhouse, http-api, plus operator-defined). " +
			"Each entry's `extra.env_vars` lists the env var names that will be stamped into the sandbox when the service is bound — read those in your code. " +
			"USE BEFORE writing code that talks to an external service so you know the canonical env var names and what fields the user has to supply.",
	}, serviceList(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "service_bind",
		Description: "Wire a cluster service (postgres, redis, openai, anthropic, …) into the sandbox. Returns the env var names the service exposes — your code reads those at runtime. " +
			"Credentials come from the user (passed in env or via the dashboard); never invent connection strings or API keys. " +
			"Code reads env vars (e.g. os.environ['DATABASE_URL']) — DO NOT hardcode connection strings or secrets in source.",
	}, serviceBind(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "secret_set",
		Description: "Set a non-secret env var the app needs (e.g. APP_ENV=production, FEATURE_FLAG_X=true). " +
			"REJECTS values matching common secret patterns (sk-*, AKIA*, JWT shape, etc) with code B010 — for those, tell the user to run `agentry env set <NAME>` in their terminal so the value never enters chat context.",
	}, secretSet(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "secret_list",
		Description: "List names of env vars + secrets staged in the sandbox (NEVER returns values). " +
			"Use to know what's available before writing code that reads from env.",
	}, secretList(c))
	// — Shell ───────────────────────────────────────────────────────────
	//
	// (Build + deploy tools removed. Deploy lives in the dashboard
	// today; the LLM should tell the user to click Share for a quick
	// dev preview or Deploy for a durable prod URL — see the
	// server-instructions "access from the user's browser" block. A
	// fresh MCP-driven deploy tool will land once we have a token-
	// auth path from the MCP client to agentry-app.)
	mcp.AddTool(server, &mcp.Tool{
		Name: "command_run",
		Description: "Run a BLOCKING shell command and wait for stdout/exit_code. Reuse `session_id` across calls for a persistent bash PTY (keeps cwd/env). " +
			"USE FOR: pip/npm installs, pytest, curl, git, build/deploy commands. " +
			"DO NOT USE FOR: writing files (use `file_write` — 5 ms vs 100-300 ms per heredoc) or long-running servers (use `command_start` or `project_start`). " +
			"TIMEOUT: pick `timeout` deliberately — full rubric on the schema, but the headline is 300+ for pip/npm install, 900 for docker build. A `hard_timeout` status is NOT a tunnel failure; retry with a higher timeout before giving up.",
	}, commandRun(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "command_start",
		Description: "Spawn a background shell command. Returns an `id` for command_logs / command_interrupt. " +
			"USE FOR: truly throwaway watchers you'll kill in the same chat (tail -f, one-off stress loops). " +
			"DO NOT USE FOR: any server you'll iterate on across turns — those want `project_start` (auto-restart, health checks, depends_on, port discovery).",
	}, commandStart(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "command_logs",
		Description: "Inspect background commands. " +
			"With no `id`: list every background command with status snapshots. " +
			"With `id`: return that command's status PLUS new stdout/stderr since byte `cursor` (pass back the returned cursor to incrementally tail).",
	}, commandLogs(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "command_interrupt",
		Description: "Stop a background command by id: SIGTERM the process group, escalate to SIGKILL after 5 s grace.",
	}, commandInterrupt(c))

	// — Files ───────────────────────────────────────────────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_read",
		Description: "Read a file. Output is line-numbered (cat -n style) by default so you can refer to specific lines in follow-up edits without inventing anchors. The response includes `total_lines` — compare to your slice's end_line to know if you saw the whole file. " +
			"Pass `start_line`+`end_line` (both 1-based, inclusive) to read a window of a large file; the runtime streams line-by-line so a 10k-line file doesn't pay to load all 10k for a 50-line slice. " +
			"Set `format: \"raw\"` to drop the line numbers (useful when piping into another tool). " +
			"Returns 403 on /etc/sandbox/creds/* — read those from app code, not via this tool.",
	}, fileRead(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_write",
		Description: "Write a file (atomic overwrite via tmp+rename by default; `append=true` to extend). Creates parent dirs automatically — no `mkdir -p` needed. " +
			"THIS IS THE TOOL FOR CREATING FILES. Heredocs via `command_run` (`cat > x <<EOF`, `tee`, `printf > x`, `echo > x`, `python3 -c open(...).write(...)`) are FORBIDDEN — 100-300 ms PTY round-trip vs ~5 ms direct os.WriteFile here. " +
			"For editing an existing file, prefer `file_replace` (one change) or `file_multi_edit` (several changes, atomic) — both are diff-sized over the wire instead of resending the whole file. " +
			"DO NOT write `.sandbox-project.json` directly — use `project_create` (it picks the right start_command for your kind, scaffolds the starter files, and gets the lifecycle right). file_write to that path bypasses the project pattern and produces sandboxes the LLM can't reliably operate.",
	}, fileWrite(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_list",
		Description: "Enumerate paths under a directory. " +
			"`recursive=true` walks subdirs. " +
			"`glob` (e.g. `**/*.py`, `src/*.tsx`, `**/*.{ts,tsx,json}`) filters to matching paths. Doublestar globs and brace alternation are both supported. " +
			"For content search across many files, use `file_grep`; for inside one file, use `file_search`.",
	}, fileList(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "docs_read",
		Description: "Read a baked-in operator cheat-sheet from /etc/sandbox/docs/. PREFER this over file_read or command_run (`cat`, `find`) when reaching for a doc — the docs are INSIDE the sandbox container, NOT on your host. NEVER run `find /` to locate them.\n\n" +
			"Available names (you can omit the .md suffix):\n" +
			"  • README                                 — the recipe router (read first)\n" +
			"  • coding-style                           — house code rules\n" +
			"  • projects                               — project manifest schema\n" +
			"  • app                                    — Next.js recipe\n" +
			"  • static-html                            — vanilla HTML recipe\n" +
			"  • streamlit                              — Streamlit recipe\n" +
			"  • fastapi                                — FastAPI recipe\n" +
			"  • python-script                          — long-running Python recipe\n" +
			"  • skills/frontend-design                 — design principles; READ before any CSS/HTML/JSX\n" +
			"  • skills/frontend-design/references/ai-tells     — AI-slop patterns to refuse\n" +
			"  • skills/frontend-design/references/design-rules — hard typographic/color rules\n" +
			"  • skills/theme-factory                   — themes index\n" +
			"  • skills/theme-factory/styles-catalog    — 50+ named styles (bento, neobrutalist, sekiro, …)\n" +
			"  • skills/theme-factory/themes/<theme>    — one of: arctic-frost, botanical-garden, desert-rose, forest-canopy, golden-hour, midnight-galaxy, modern-minimalist, ocean-depths, sunset-boulevard, tech-innovation\n" +
			"  • skills/brand-guidelines                — brand consistency\n" +
			"  • skills/webapp-testing                  — Playwright + screenshot testing\n" +
			"  • skills/web-artifacts-builder           — single-file HTML artifacts",
	}, docsRead(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_grep",
		Description: "Multi-file regex search. Walks `path`, optionally filtering by `glob` (doublestar; e.g. `**/*.ts`), skips binary files, and returns structured `{file, line, text}` matches with optional `context_before`/`context_after` lines. " +
			"Default cap is 200 matches; `truncated: true` in the response means there were more. " +
			"PREFER over `command_run` + grep — structured output, no shell parsing, sandbox-side filtering.",
	}, fileGrep(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_replace",
		Description: "Replace literal `old_str` with `new_str` in one file. STRICT BY DEFAULT: if `old_str` matches more than once, the call ERRORS with the actual count instead of clobbering every site. " +
			"To rename across all occurrences, pass `replace_all: true`. To assert a specific count (paranoia mode), pass `expected_matches: N`. " +
			"Atomic write via temp file + rename — a crash mid-write can't leave a corrupted source file. " +
			"For multiple edits to the SAME file, prefer `file_multi_edit` — one round-trip instead of N, all-or-nothing.",
	}, fileReplace(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "file_multi_edit",
		Description: "Apply several edits to ONE file in a single atomic call. Each edit is a `{old_str, new_str, expected_matches?, replace_all?}` step with the same strict semantics as `file_replace`. Steps run sequentially — each operates on the result of the previous — and the file is written ONLY if every step succeeds (all-or-nothing). " +
			"USE THIS instead of N separate `file_replace` calls when refactoring: ~10× faster (one HTTP round-trip vs N) and you can't leave the file half-edited. " +
			"Max 100 edits per call.",
	}, fileMultiEdit(c))

	// — Ports ───────────────────────────────────────────────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "port_wait",
		Description: "Block until a TCP port enters LISTEN inside the sandbox, or `timeout_seconds` elapses. " +
			"Call after `command_start` on a server, before curling it.",
	}, portWait(c))

	// — Project manager ─────────────────────────────────────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "project_create",
		Description: "Scaffold a managed project at /workspace/projects/<name>/. ALWAYS use this to set up a project — writing `.sandbox-project.json` by hand via `file_write` is FORBIDDEN. " +
			"`kind` picks the template:\n" +
			"  • nextjs        — Next.js App Router; follow app.md to flesh out the actual app, then project_start\n" +
			"  • static-html   — vanilla HTML/CSS/JS served by `python3 -m http.server`. The right choice for landing pages, marketing, microsites, portfolios — anything where you'd otherwise tell the user to run a static server.\n" +
			"  • streamlit     — Python data app; scaffolds app.py + requirements.txt + start_command\n" +
			"  • fastapi       — Python API; scaffolds app.py + requirements.txt + uvicorn start_command\n" +
			"  • python-script — long-running Python process (worker, batch job); scaffolds main.py\n" +
			"  • custom        — bring your own start_command (REQUIRED argv array when kind=custom)\n" +
			"Scaffold returns the manifest path + the starter files written + the start_command + a next_step hint. Stubs are minimal — overwrite them. " +
			"NEVER tell the user to run a server themselves (no `python3 -m http.server` instructions, no `npm run dev`, no `streamlit run` — the project manager owns the lifecycle, ports, restarts, and logs).",
	}, projectCreate(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "project_start",
		Description: "Start the sandbox's managed project. Reads `/workspace/projects/<name>/.sandbox-project.json` (scaffolded by `project_create`). `restart=true` stop+starts. " +
			"DEFAULT this over command_start for any server the user will touch more than once — bare command_start doesn't get auto-restart, port discovery, or log capture.",
	}, projectStart(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_stop",
		Description: "Stop a managed project (SIGTERM the process group, SIGKILL after grace).",
	}, projectStop(c))
	mcp.AddTool(server, &mcp.Tool{
		Name: "project_list",
		Description: "List managed projects with status, pid, discovered `ports[]`, uptime, restart_count, health, last_error. " +
			"Self-check: after declaring you're done, this should show every project running with non-empty ports.",
	}, projectList(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_logs",
		Description: "Recent log buffer (~500 lines) for a managed project. Stdout and stderr interleaved in order.",
	}, projectLogs(c))

	// — Code interpreter (Jupyter) ──────────────────────────────────────
	mcp.AddTool(server, &mcp.Tool{
		Name: "code_exec",
		Description: "Run Python statefully in a kernel. Pass any `context_id` (new = auto-spawned kernel; reused = same state across calls). " +
			"Returns: `context_id` (echo back for next call), `stdout`, `stderr`, `result` (last expression as MIME dict), `displays` (matplotlib/plotly come back as renderable inline images). " +
			"USE FOR: exploration — SHOW CATALOGS, df.head(), a quick histogram. ≤30 lines per call. " +
			"Code you want to KEEP belongs in `file_write` under /workspace/projects/<name>/, not in a kernel that dies with the sandbox.",
	}, codeExec(c))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "code_close",
		Description: "Shut down a kernel context and free its memory. Idempotent. Call when you're done with a context — each kernel is a real Python process.",
	}, codeClose(c))
}

// --- arg structs ----------------------------------------------------------

type sandboxCreateArgs struct {
	SandboxID    string `json:"sandbox_id" jsonschema:"requested name (descriptive). If taken, server auto-suffixes — use the sandbox_id from the response, not this value, for follow-up calls"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty" jsonschema:"reaper deadline in seconds; 0 = no TTL"`
	RuntimeClass string `json:"runtime_class,omitempty" jsonschema:"Kubernetes RuntimeClass (gvisor / kata / firecracker)"`

	// ReuseExisting opts into the rare "attach to whatever sandbox
	// already has this name" semantics. Almost always false for an
	// LLM-driven flow — see the tool description above.
	ReuseExisting bool `json:"reuse_existing,omitempty" jsonschema:"true ONLY when intentionally re-attaching to an existing sandbox by name; default false"`
}

type sandboxListArgs struct {
	SandboxID string `json:"sandbox_id,omitempty" jsonschema:"optional: pass to describe one sandbox; omit to list all"`
}

type sandboxIDArgs struct {
	SandboxID string `json:"sandbox_id" jsonschema:"the sandbox id"`
}

type serviceListArgs struct {
	Kind string `json:"kind,omitempty" jsonschema:"optional filter: service | skill. Default returns services."`
}

type serviceBindArgs struct {
	SandboxID string `json:"sandbox_id" jsonschema:"the sandbox to wire the service into"`
	Service   string `json:"service" jsonschema:"service name from the catalog (e.g. postgres, redis, openai)"`
	Version   string `json:"version,omitempty" jsonschema:"optional version pin; default = latest in catalog"`
}

type secretSetArgs struct {
	SandboxID string `json:"sandbox_id" jsonschema:"the sandbox to set the env on"`
	Name      string `json:"name" jsonschema:"env var name; must match [A-Z_][A-Z0-9_]*"`
	Value     string `json:"value" jsonschema:"value (rejected if it looks like a secret — use agentry env set on terminal for those)"`
}

type sandboxIDOnlyArgs struct {
	SandboxID string `json:"sandbox_id" jsonschema:"the sandbox id"`
}

type authSetupArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Project    string `json:"project,omitempty" jsonschema:"project directory under /workspace/projects/. Default: 'app'. Most apps have one project so this can usually be omitted."`
	Mode       string `json:"mode,omitempty" jsonschema:"empty = phase 1 (probe + return questions for the user); 'none' / 'sqlite' / 'binding:postgres' / 'binding:mongodb' = phase 2 (deterministic scaffold). Pass the value the user chose verbatim."`
}

type commandRunArgs struct {
	SandboxURL string  `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Command    string  `json:"command" jsonschema:"the shell command to execute"`
	SessionID  string  `json:"session_id,omitempty" jsonschema:"persistent bash PTY id; same id keeps cwd & env"`
	ExecDir    string  `json:"exec_dir,omitempty" jsonschema:"working directory for the command"`
	Timeout    float64 `json:"timeout,omitempty" jsonschema:"per-call timeout in seconds. Default 120 covers most work; pick deliberately for known-slow commands. Rubric — quick checks (ls, cat, ps, git status, curl with --max-time): 30. Tests / one-off builds (pytest, go build, cargo build small): 120 (default). Package installs (pip install -r, npm install, apt-get install): 300; bump to 600 for heavy ML / langchain / full node_modules trees. Container builds (docker build, buildah): 900. Multi-minute data jobs / migrations: set explicitly to your best estimate × 1.5. NEVER bail on a hard_timeout status without first retrying with a higher value — the tunnel is fine; only the per-call budget expired."`
}

type commandStartArgs struct {
	SandboxURL string            `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Command    string            `json:"command" jsonschema:"the shell command to spawn (e.g. 'python3 -m http.server 8000')"`
	ExecDir    string            `json:"exec_dir,omitempty" jsonschema:"working directory; defaults to /workspace"`
	Env        map[string]string `json:"env,omitempty" jsonschema:"extra env vars to set"`
}

type commandLogsArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	ID         string `json:"id,omitempty" jsonschema:"optional: omit to list all background commands; pass an id to get status + logs for one"`
	Cursor     int64  `json:"cursor,omitempty" jsonschema:"byte offset to start reading from (only used when id is set); pass back the cursor from the previous call"`
}

type commandIDArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	ID         string `json:"id" jsonschema:"the background command id returned by command_start"`
}

type docsReadArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Name       string `json:"name" jsonschema:"doc name relative to /etc/sandbox/docs/, with or without the .md suffix (e.g. 'README', 'app', 'skills/frontend-design', 'skills/theme-factory/themes/sunset-boulevard'). See the tool description for the full list."`
}

type fileReadArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	File       string `json:"file" jsonschema:"absolute file path inside the sandbox"`
	StartLine  int    `json:"start_line,omitempty" jsonschema:"1-based start line (optional)"`
	EndLine    int    `json:"end_line,omitempty" jsonschema:"inclusive end line (optional)"`
	Format     string `json:"format,omitempty" jsonschema:"'numbered' (default, cat -n style — refer to lines verbatim) or 'raw' (no line numbers)"`
}

type fileWriteArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	File       string `json:"file" jsonschema:"absolute file path inside the sandbox"`
	Content    string `json:"content" jsonschema:"file content to write"`
	Append     bool   `json:"append,omitempty" jsonschema:"true to append instead of overwrite"`
}

type fileListArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Path       string `json:"path" jsonschema:"directory path inside the sandbox"`
	Recursive  bool   `json:"recursive,omitempty" jsonschema:"recurse into subdirectories"`
	Glob       string `json:"glob,omitempty" jsonschema:"optional glob filter (e.g. '**/*.py'); when set, results contain only matching paths"`
}

type fileReplaceArgs struct {
	SandboxURL      string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	File            string `json:"file" jsonschema:"absolute file path"`
	OldStr          string `json:"old_str" jsonschema:"literal string to find. Must be UNIQUE in the file by default — if it isn't, the call errors with the actual match count. Add surrounding context (or set replace_all) to make it unambiguous."`
	NewStr          string `json:"new_str" jsonschema:"replacement"`
	ReplaceAll      bool   `json:"replace_all,omitempty" jsonschema:"true to opt into clobbering every occurrence of old_str (renames, bulk updates). Default is strict single-match."`
	ExpectedMatches int    `json:"expected_matches,omitempty" jsonschema:"assert old_str appears exactly N times before touching the file; the call errors if the actual count differs. Use when you know the count up-front (e.g. from a prior file_grep)."`
}

type fileMultiEditArgs struct {
	SandboxURL string             `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	File       string             `json:"file" jsonschema:"absolute file path"`
	Edits      []fileMultiEditOp  `json:"edits" jsonschema:"ordered list of edits to apply. Each step operates on the result of the previous; the file is written only if every step succeeds."`
}

type fileMultiEditOp struct {
	OldStr          string `json:"old_str" jsonschema:"literal string to find (must be unique by default — same semantics as file_replace)"`
	NewStr          string `json:"new_str" jsonschema:"replacement"`
	ReplaceAll      bool   `json:"replace_all,omitempty" jsonschema:"true to opt into clobbering every occurrence in this step"`
	ExpectedMatches int    `json:"expected_matches,omitempty" jsonschema:"assert N matches for this step's old_str"`
}

type fileGrepArgs struct {
	SandboxURL    string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Path          string `json:"path" jsonschema:"directory to walk (e.g. '/workspace/projects/app/src')"`
	Regex         string `json:"regex" jsonschema:"RE2 regular expression matched against each line"`
	Glob          string `json:"glob,omitempty" jsonschema:"doublestar glob filter applied to file paths relative to path (e.g. '**/*.ts', '**/*.{ts,tsx,json}'). Omit to scan every text file under path."`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"cap on returned matches (default 200). total_found in the response is the uncapped count."`
	ContextBefore int    `json:"context_before,omitempty" jsonschema:"lines of context BEFORE each match (default 0)"`
	ContextAfter  int    `json:"context_after,omitempty" jsonschema:"lines of context AFTER each match (default 0)"`
}

type portWaitArgs struct {
	SandboxURL     string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Port           int    `json:"port" jsonschema:"TCP port to wait for"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"max wait in seconds; default 30"`
}

type projectCreateArgs struct {
	SandboxURL   string   `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Name         string   `json:"name" jsonschema:"project directory name under /workspace/projects/. Use the deliverable's short name (e.g. 'landing', 'jira-dash'). One path segment, no slashes."`
	Kind         string   `json:"kind" jsonschema:"template to scaffold: nextjs | static-html | streamlit | fastapi | python-script | custom"`
	StartCommand []string `json:"start_command,omitempty" jsonschema:"argv array. REQUIRED only when kind=custom; ignored otherwise. Example: ['node', 'server.js']"`
	Port         int      `json:"port,omitempty" jsonschema:"override the default port for the kind (static-html 8000, streamlit 8501, fastapi 8000). Most kinds work fine on defaults."`
}

type projectStartArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Name       string `json:"name" jsonschema:"project name (the directory under /workspace/projects containing .sandbox-project.json)"`
	Restart    bool   `json:"restart,omitempty" jsonschema:"if true and the project is already running, stop then start it"`
}

type projectNameArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	Name       string `json:"name" jsonschema:"project name (the directory under /workspace/projects containing .sandbox-project.json)"`
}

type sandboxURLOnlyArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
}

type codeExecArgs struct {
	SandboxURL     string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	ContextID      string `json:"context_id,omitempty" jsonschema:"kernel id; new id = auto-spawn; omit = auto-generate. Reuse the returned context_id to keep state."`
	Code           string `json:"code" jsonschema:"Python source. The last line, if it's an expression, becomes the structured 'result' in the response."`
	TimeoutSeconds int    `json:"timeout,omitempty" jsonschema:"max wall-clock for this execution in seconds (default 30)"`
}

type codeContextIDArgs struct {
	SandboxURL string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the URL of the most recent sandbox_create in this MCP session. Pass explicitly only when juggling multiple sandboxes."`
	ContextID  string `json:"context_id" jsonschema:"the context id to close"`
}

// --- handlers -------------------------------------------------------------

func sandboxCreate(c *Client) mcp.ToolHandlerFor[sandboxCreateArgs, SandboxCreateResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxCreateArgs) (*mcp.CallToolResult, SandboxCreateResult, error) {
		if a.SandboxID == "" {
			return errResult("sandbox_id is required"), SandboxCreateResult{}, nil
		}
		info, err := c.CreateSandbox(ctx, CreateRequest{
			SandboxID:     a.SandboxID,
			TTLSeconds:    a.TTLSeconds,
			RuntimeClass:  a.RuntimeClass,
			ReuseExisting: a.ReuseExisting,
		})
		if err != nil {
			return errResult(err.Error()), SandboxCreateResult{}, nil
		}
		// Cache the runtime URL so every later file/command/project/
		// port/code tool in this MCP session can omit sandbox_url and
		// fall back to this value. Saves ~60 tokens per call and gets
		// rid of the awkward bridge.invalid URL the LLM kept having
		// to rationalize about.
		c.SetLastSandboxURL(info.SandboxURL)
		// Best-effort: apply any cluster-default service binds the
		// user staged via `agentry service bind <service>`. A failure
		// here doesn't void the create — the LLM still gets a
		// working sandbox, just without the auto-applied creds.
		if c.PostCreateHook != nil {
			if hookErr := c.PostCreateHook(ctx, info); hookErr != nil {
				log.Printf("sandbox_create: post-create hook: %v", hookErr)
			}
		}
		// Re-list bindings AFTER the hook so the response shows what's
		// actually wired up. The LLM uses this to skip in-process
		// substitutes (mongodb-memory-server, embedded sqlite) when a
		// real cluster service is already bound. Failure is non-fatal:
		// an empty bindings array is "I don't know" not "no bindings".
		bindings, listErr := c.ListBindings(ctx, info.SandboxID)
		if listErr != nil {
			log.Printf("sandbox_create: list-bindings post-hook: %v", listErr)
		}
		// Same idea for staged env vars / secrets — surface the NAMES
		// (the values never leave the sandbox) so the LLM can match a
		// user-named service (JIRA, Slack, …) against what's already
		// loaded before declaring a credential is missing. The list
		// endpoint is names-only by contract.
		envNames, envErr := c.ListSecrets(ctx, info.SandboxID)
		if envErr != nil {
			log.Printf("sandbox_create: list-secrets post-hook: %v", envErr)
		}
		result := SandboxCreateResult{SandboxInfo: info, Bindings: bindings, Env: envNames}
		return jsonResult(result), result, nil
	}
}

// sandboxListEntry is the trimmed shape sandbox_list returns when no
// sandbox_id is given — deliberately NO sandbox_url. Returning URLs
// by default invites the LLM to grab any URL it sees and start
// writing files there, even if it didn't create that sandbox. Forcing
// a follow-up sandbox_list?sandbox_id=… to get a URL turns "browse +
// reuse" into an explicit, named "I'm targeting THIS one" gesture.
type sandboxListEntry struct {
	SandboxID string `json:"sandbox_id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// sandboxList is polymorphic: with sandbox_id present, returns one
// sandbox's full details (incl. sandbox_url); without, returns the
// trimmed entries above. Output type is `any` so the JSON shape can
// flex between array and single object.
func sandboxList(c *Client) mcp.ToolHandlerFor[sandboxListArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxListArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxID != "" {
			info, err := c.GetSandbox(ctx, a.SandboxID)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			return jsonResult(info), info, nil
		}
		list, err := c.ListSandboxes(ctx)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		entries := make([]sandboxListEntry, len(list))
		for i, s := range list {
			entries[i] = sandboxListEntry{
				SandboxID: s.SandboxID,
				Status:    s.Status,
				ExpiresAt: s.ExpiresAt,
			}
		}
		// Inline warning is on purpose — when the LLM is mid-mistake
		// (sandbox_create failed, falling back to list+pick) this note
		// reaches it at the exact moment it would otherwise pick a
		// random sandbox to write into.
		out := map[string]any{
			"sandboxes": entries,
			"note":      "sandbox_url is intentionally NOT returned here. These are OTHER projects — writing into one destroys someone else's work. For a NEW task call sandbox_create. Only if the user EXPLICITLY names a sandbox here (e.g. \"open my-store again\") should you call sandbox_list with that sandbox_id to get its URL.",
		}
		return jsonResult(out), out, nil
	}
}


func buildImage(c *Client) mcp.ToolHandlerFor[sandboxIDOnlyArgs, BuildResponse] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxIDOnlyArgs) (*mcp.CallToolResult, BuildResponse, error) {
		if a.SandboxID == "" {
			return errResult("sandbox_id is required"), BuildResponse{}, nil
		}
		resp, err := c.Build(ctx, a.SandboxID)
		if err != nil {
			return errResult(err.Error()), resp, nil
		}
		return jsonResult(resp), resp, nil
	}
}

func secretSet(c *Client) mcp.ToolHandlerFor[secretSetArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a secretSetArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxID == "" || a.Name == "" {
			return errResult("sandbox_id and name are both required"), nil, nil
		}
		if err := c.SetSecret(ctx, a.SandboxID, a.Name, a.Value, "mcp"); err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(map[string]any{"name": a.Name, "ok": true}), nil, nil
	}
}

func secretList(c *Client) mcp.ToolHandlerFor[sandboxIDOnlyArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxIDOnlyArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxID == "" {
			return errResult("sandbox_id is required"), nil, nil
		}
		names, err := c.ListSecrets(ctx, a.SandboxID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		out := map[string]any{"names": names}
		return jsonResult(out), out, nil
	}
}

func serviceBind(c *Client) mcp.ToolHandlerFor[serviceBindArgs, BindingResponse] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a serviceBindArgs) (*mcp.CallToolResult, BindingResponse, error) {
		if a.SandboxID == "" || a.Service == "" {
			return errResult("sandbox_id and service are both required"), BindingResponse{}, nil
		}
		resp, err := c.BindService(ctx, a.SandboxID, a.Service, a.Version)
		if err != nil {
			return errResult(err.Error()), resp, nil
		}
		return jsonResult(resp), resp, nil
	}
}

func serviceList(c *Client) mcp.ToolHandlerFor[serviceListArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a serviceListArgs) (*mcp.CallToolResult, any, error) {
		kind := a.Kind
		if kind == "" {
			kind = "service" // default: just the cluster services, not dev-deps + skills
		}
		entries, err := c.ListCatalog(ctx, kind)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		out := map[string]any{"entries": entries, "kind": kind}
		return jsonResult(out), out, nil
	}
}

func authSetup(c *Client) mcp.ToolHandlerFor[authSetupArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a authSetupArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" {
			return errResult("sandbox_url is required"), nil, nil
		}
		out, err := c.AuthSetup(ctx, a.SandboxURL, AuthSetupRequest{
			Project: a.Project,
			Mode:    a.Mode,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func sandboxDelete(c *Client) mcp.ToolHandlerFor[sandboxIDArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxIDArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxID == "" {
			return errResult("sandbox_id is required"), nil, nil
		}
		if err := c.DeleteSandbox(ctx, a.SandboxID); err != nil {
			return errResult(err.Error()), nil, nil
		}
		out := map[string]any{"ok": true, "sandbox_id": a.SandboxID}
		return jsonResult(out), out, nil
	}
}

func commandRun(c *Client) mcp.ToolHandlerFor[commandRunArgs, ExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a commandRunArgs) (*mcp.CallToolResult, ExecResult, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Command == "" {
			return errResult("sandbox_url and command are required"), ExecResult{}, nil
		}
		out, err := c.Exec(ctx, a.SandboxURL, ExecRequest{
			Command: a.Command, ID: a.SessionID, ExecDir: a.ExecDir, Timeout: a.Timeout,
		})
		if err != nil {
			return errResult(err.Error()), out, nil
		}
		return jsonResult(out), out, nil
	}
}

func commandStart(c *Client) mcp.ToolHandlerFor[commandStartArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a commandStartArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Command == "" {
			return errResult("sandbox_url and command are required"), nil, nil
		}
		out, err := c.BgStart(ctx, a.SandboxURL, BgStartRequest{
			Command: a.Command, ExecDir: a.ExecDir, Env: a.Env,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

// commandLogs is polymorphic: with id present, returns one command's
// status + new log bytes since cursor. Without id, returns the list of
// all background commands with their current status snapshots.
func commandLogs(c *Client) mcp.ToolHandlerFor[commandLogsArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a commandLogsArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" {
			return errResult("sandbox_url is required"), nil, nil
		}
		if a.ID == "" {
			out, err := c.BgList(ctx, a.SandboxURL)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			return jsonResult(out), out, nil
		}
		status, err := c.BgStatus(ctx, a.SandboxURL, a.ID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		logs, err := c.BgLogs(ctx, a.SandboxURL, a.ID, a.Cursor)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		out := map[string]any{"status": status, "logs": logs}
		return jsonResult(out), out, nil
	}
}

func commandInterrupt(c *Client) mcp.ToolHandlerFor[commandIDArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a commandIDArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.ID == "" {
			return errResult("sandbox_url and id are required"), nil, nil
		}
		out, err := c.BgInterrupt(ctx, a.SandboxURL, a.ID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func docsRead(c *Client) mcp.ToolHandlerFor[docsReadArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a docsReadArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Name == "" {
			return errResult("sandbox_url and name are required"), nil, nil
		}
		// Normalise: strip leading /, append .md if missing, refuse
		// path traversal so the LLM can't dodge out of /etc/sandbox/docs
		// into /etc/sandbox/creds via "../creds/...".
		name := strings.TrimLeft(a.Name, "/")
		if strings.Contains(name, "..") {
			return errResult("docs name must not contain '..'"), nil, nil
		}
		if !strings.HasSuffix(name, ".md") {
			name += ".md"
		}
		path := "/etc/sandbox/docs/" + name
		out, err := c.ReadFile(ctx, a.SandboxURL, FileReadRequest{File: path, Format: "raw"})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func fileRead(c *Client) mcp.ToolHandlerFor[fileReadArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileReadArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.File == "" {
			return errResult("sandbox_url and file are required"), nil, nil
		}
		// Default to numbered output so the LLM can refer to lines by
		// number in follow-up edits. Caller can opt out with "raw".
		format := a.Format
		if format == "" {
			format = "numbered"
		}
		out, err := c.ReadFile(ctx, a.SandboxURL, FileReadRequest{
			File: a.File, StartLine: a.StartLine, EndLine: a.EndLine, Format: format,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func fileWrite(c *Client) mcp.ToolHandlerFor[fileWriteArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileWriteArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.File == "" {
			return errResult("sandbox_url and file are required"), nil, nil
		}
		out, err := c.WriteFile(ctx, a.SandboxURL, FileWriteRequest{
			File: a.File, Content: a.Content, Append: a.Append,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

// fileList is polymorphic on the `glob` field: when empty, it lists the
// directory; when set, it routes to the find endpoint and returns
// matching paths.
func fileList(c *Client) mcp.ToolHandlerFor[fileListArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileListArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Path == "" {
			return errResult("sandbox_url and path are required"), nil, nil
		}
		if a.Glob != "" {
			out, err := c.FindFiles(ctx, a.SandboxURL, FileFindRequest{
				Path: a.Path, Glob: a.Glob,
			})
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			return jsonResult(out), out, nil
		}
		out, err := c.ListFiles(ctx, a.SandboxURL, FileListRequest{
			Path: a.Path, Recursive: a.Recursive,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func fileReplace(c *Client) mcp.ToolHandlerFor[fileReplaceArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileReplaceArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.File == "" {
			return errResult("sandbox_url and file are required"), nil, nil
		}
		req := FileReplaceRequest{File: a.File, OldStr: a.OldStr, NewStr: a.NewStr}
		// Only forward the strict knobs when the caller actually set
		// them. nil = "use server default" (single-match strict);
		// non-nil = "the caller opted into something explicit".
		if a.ReplaceAll {
			b := true
			req.ReplaceAll = &b
		}
		if a.ExpectedMatches > 0 {
			n := a.ExpectedMatches
			req.ExpectedMatches = &n
		}
		out, err := c.ReplaceInFile(ctx, a.SandboxURL, req)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func fileMultiEdit(c *Client) mcp.ToolHandlerFor[fileMultiEditArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileMultiEditArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.File == "" {
			return errResult("sandbox_url and file are required"), nil, nil
		}
		if len(a.Edits) == 0 {
			return errResult("edits must contain at least one step"), nil, nil
		}
		steps := make([]FileEditStep, len(a.Edits))
		for i, e := range a.Edits {
			steps[i] = FileEditStep{OldStr: e.OldStr, NewStr: e.NewStr}
			if e.ReplaceAll {
				b := true
				steps[i].ReplaceAll = &b
			}
			if e.ExpectedMatches > 0 {
				n := e.ExpectedMatches
				steps[i].ExpectedMatches = &n
			}
		}
		out, err := c.MultiEditFile(ctx, a.SandboxURL, FileMultiEditRequest{
			File:  a.File,
			Edits: steps,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func fileGrep(c *Client) mcp.ToolHandlerFor[fileGrepArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a fileGrepArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Path == "" || a.Regex == "" {
			return errResult("sandbox_url, path, and regex are required"), nil, nil
		}
		req := FileGrepRequest{Path: a.Path, Regex: a.Regex, Glob: a.Glob}
		if a.MaxResults > 0 {
			n := a.MaxResults
			req.MaxResults = &n
		}
		if a.ContextBefore > 0 {
			n := a.ContextBefore
			req.ContextBefore = &n
		}
		if a.ContextAfter > 0 {
			n := a.ContextAfter
			req.ContextAfter = &n
		}
		out, err := c.GrepFiles(ctx, a.SandboxURL, req)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func portWait(c *Client) mcp.ToolHandlerFor[portWaitArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a portWaitArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Port == 0 {
			return errResult("sandbox_url and port are required"), nil, nil
		}
		out, err := c.PortWait(ctx, a.SandboxURL, PortWaitRequest{
			Port: a.Port, TimeoutSeconds: a.TimeoutSeconds,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func projectCreate(c *Client) mcp.ToolHandlerFor[projectCreateArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a projectCreateArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Name == "" || a.Kind == "" {
			return errResult("sandbox_url, name, and kind are required"), nil, nil
		}
		out, err := c.ProjectCreate(ctx, a.SandboxURL, ProjectCreateRequest{
			Name:         a.Name,
			Kind:         a.Kind,
			StartCommand: a.StartCommand,
			Port:         a.Port,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func projectStart(c *Client) mcp.ToolHandlerFor[projectStartArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a projectStartArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Name == "" {
			return errResult("sandbox_url and name are required"), nil, nil
		}
		var (
			out map[string]any
			err error
		)
		if a.Restart {
			out, err = c.ProjectRestart(ctx, a.SandboxURL, a.Name)
		} else {
			out, err = c.ProjectStart(ctx, a.SandboxURL, a.Name)
		}
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func projectStop(c *Client) mcp.ToolHandlerFor[projectNameArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a projectNameArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Name == "" {
			return errResult("sandbox_url and name are required"), nil, nil
		}
		out, err := c.ProjectStop(ctx, a.SandboxURL, a.Name)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func projectList(c *Client) mcp.ToolHandlerFor[sandboxURLOnlyArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a sandboxURLOnlyArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" {
			return errResult("sandbox_url is required"), nil, nil
		}
		out, err := c.ProjectList(ctx, a.SandboxURL)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

func projectLogs(c *Client) mcp.ToolHandlerFor[projectNameArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a projectNameArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Name == "" {
			return errResult("sandbox_url and name are required"), nil, nil
		}
		out, err := c.ProjectLogs(ctx, a.SandboxURL, a.Name)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

// --- code interpreter handlers ────────────────────────────────────────────

// maxInlineImages caps how many rendered images we return per code_exec
// call. Images are ~50-500 KB each; uncapped responses can blow up the
// model's context. Excess images are still visible in the JSON via
// their marker — the model knows they exist, just doesn't see them.
const maxInlineImages = 8

// codeExec runs Python in a kernel. If context_id is empty or unknown,
// a kernel is auto-spawned (no separate create call needed). The
// returned `context_id` is echoed back so the model can reuse it for
// state continuity.
func codeExec(c *Client) mcp.ToolHandlerFor[codeExecArgs, CodeExecResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a codeExecArgs) (*mcp.CallToolResult, CodeExecResult, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Code == "" {
			return errResult("sandbox_url and code are required"), CodeExecResult{}, nil
		}
		// Auto-create the kernel context when the model doesn't pass
		// one (or passes a new id). The list endpoint isn't cheap, so
		// we just try to create — the runtime is idempotent on a known
		// id and otherwise hands us a fresh kernel.
		contextID := a.ContextID
		if contextID == "" {
			contextID = newContextID()
		}
		if _, err := c.CreateContext(ctx, a.SandboxURL, CodeContextCreateRequest{
			ContextID: contextID,
		}); err != nil {
			// "already exists" is fine — that's the reuse path.
			if !strings.Contains(err.Error(), "already exists") &&
				!strings.Contains(err.Error(), "exists") {
				return errResult(err.Error()), CodeExecResult{}, nil
			}
		}
		res, err := c.ExecCode(ctx, a.SandboxURL, contextID, CodeExecRequest{
			Code: a.Code, TimeoutSeconds: a.TimeoutSeconds,
		})
		if err != nil {
			return errResult(err.Error()), res, nil
		}
		res.ContextID = contextID

		stripped, images := extractRenderableImages(res)
		text, _ := json.MarshalIndent(stripped, "", "  ")
		content := []mcp.Content{&mcp.TextContent{Text: string(text)}}
		content = append(content, images...)
		return &mcp.CallToolResult{Content: content}, stripped, nil
	}
}

func codeClose(c *Client) mcp.ToolHandlerFor[codeContextIDArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a codeContextIDArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.ContextID == "" {
			return errResult("sandbox_url and context_id are required"), nil, nil
		}
		out, err := c.DeleteContext(ctx, a.SandboxURL, a.ContextID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(out), out, nil
	}
}

// newContextID returns a short random id suitable for a Jupyter kernel
// label. 8 hex chars = 32 bits, plenty for distinguishing kernels in
// one sandbox; falls back to a deterministic-ish string on the (very
// rare) failure of crypto/rand.
func newContextID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ctx-fallback"
	}
	return "ctx-" + hex.EncodeToString(b[:])
}

// extractRenderableImages walks every MIME-typed payload in `res.Result`
// and `res.Displays[*].data`, pulls out PNG/JPEG bytes as ImageContent
// blocks, and REPLACES the heavy strings in the result with short
// markers so the JSON serialization stays compact.
func extractRenderableImages(res CodeExecResult) (CodeExecResult, []mcp.Content) {
	var out []mcp.Content

	processBundle := func(data map[string]any) {
		for _, mime := range renderableImageMIMEs {
			if len(out) >= maxInlineImages {
				return
			}
			raw, ok := data[mime].(string)
			if !ok || raw == "" {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				continue
			}
			out = append(out, &mcp.ImageContent{Data: decoded, MIMEType: mime})
			data[mime] = fmt.Sprintf("<%s rendered as image #%d>", mime, len(out))
		}
	}

	if res.Result != nil {
		processBundle(res.Result)
	}
	for i := range res.Displays {
		if d, ok := res.Displays[i]["data"].(map[string]any); ok {
			processBundle(d)
		}
	}

	totalImages := countImageBundles(res)
	if totalImages > len(out) {
		hint := fmt.Sprintf("\n[ad-sandbox] %d of %d images rendered inline; the rest are referenced in JSON but not attached as content.\n",
			len(out), totalImages)
		res.Stdout += hint
	}
	return res, out
}

// renderableImageMIMEs are MIME types Claude (and most MCP hosts) can
// display inline. SVG is handled as text; PDF is skipped.
var renderableImageMIMEs = []string{"image/png", "image/jpeg"}

// countImageBundles counts every PNG/JPEG payload across result +
// displays, before any extraction. Used for the cap-hint message.
func countImageBundles(res CodeExecResult) int {
	count := func(data map[string]any) int {
		n := 0
		for _, m := range renderableImageMIMEs {
			if v, ok := data[m].(string); ok && v != "" {
				n++
			}
		}
		return n
	}
	total := 0
	if res.Result != nil {
		total += count(res.Result)
	}
	for _, d := range res.Displays {
		if dd, ok := d["data"].(map[string]any); ok {
			total += count(dd)
		}
	}
	return total
}

// --- result helpers -------------------------------------------------------

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf("%v", v))
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
