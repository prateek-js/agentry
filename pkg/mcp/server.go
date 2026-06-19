package mcp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is reported to MCP clients during the handshake.
const Version = "1.0.0"

// serverInstructions is surfaced to MCP clients in the `initialize`
// response and propagated by Claude Desktop into the model's
// system-level context for every chat. It tells the model WHEN to
// reach for agentry — i.e. the cue words that should make
// `sandbox_create` the first move rather than a fallback.
//
// Keep this short and concrete. The model already has detailed tool
// descriptions; this exists to bias *which* tool it picks first.
const serverInstructions = `agentry is an isolated execution environment with credentials, build tooling, and a managed-project runtime. Call sandbox_create as the FIRST tool whenever the user asks to build a dashboard, web app, landing page, marketing site, portfolio, conference page, internal tool, or "make it look like X" — any visual deliverable, OR to run real code against real data, APIs, or credentials.

CLARIFY BEFORE BUILDING. Ask 1-3 short questions and WAIT for answers before sandbox_create / file_write. Never more than 3.

ONE PROJECT PER SANDBOX. /workspace/projects/ contains ONE directory. Databases, queues, caches → service_bind, never as a second project.

FRESH TASK → FRESH SANDBOX. A new task is a new sandbox. ALWAYS call sandbox_create with a name from the task (e.g. "jira-dashboard"); the server auto-suffixes if the name is taken — you do NOT collide with what's there. NEVER walk sandbox_list to find a workspace to write into: every existing sandbox holds a DIFFERENT project, and writing into it destroys that work. If sandbox_create fails, REPORT the failure to the user and STOP — do not "fall back" to listing + reusing a survivor. The only time you touch an existing sandbox is when the USER explicitly names it ("keep working on jira-dash", "open my-store again").

AUTH — when the user wants accounts/login/sessions, check the sandbox's env for ` + "`AGENTRY_AUTH_ENABLED=true`" + `. If present, the agentry sidecar handles login/sessions/OAuth at the network layer — write only the middleware that reads ` + "`x-forwarded-user`" + ` / ` + "`x-forwarded-email`" + ` headers (see ` + "`docs_read(\"skills/auth\")`" + `). NEVER hand-roll bcrypt, JWT, session cookies, OAuth, in-memory user tables, or localStorage sessions. If ` + "`AGENTRY_AUTH_ENABLED`" + ` is NOT set, tell the user to run ` + "`agentry auth enable`" + ` on their CLI (it validates the DB binding and turns the sidecar on); do not scaffold a substitute.

REAL DATA, NEVER FAKE. No localStorage as primary persistence. No in-process databases (mongodb-memory-server, fakeredis, pg-mem) as substitutes for a bound service. If the user wants persistence and there's no binding, ASK for the connection URL.

SERVICE NAMED → CHECK FIRST. If the user names a service (jira, slack, stripe, openai, …), scan BOTH ` + "`bindings`" + ` AND ` + "`env`" + ` in the sandbox_create response BEFORE asking for credentials or saying it isn't configured. Match by substring on the name (JIRA_TOKEN counts for "jira"). The operator may have already staged it cluster-wide.

EXISTING GITHUB REPO → CLONE, DON'T SCAFFOLD. If the user names a GitHub repo to work on (owner/repo or a github.com URL), and ` + "`GITHUB_TOKEN`" + ` is in the sandbox env, follow ` + "`docs_read(\"skills/github\")`" + ` — clone the repo into ` + "`/workspace/projects/<repo-name>`" + `, then ` + "`project_create`" + ` with the matching kind (don't let project_create scaffold over real files — it skips existing ones by design). If ` + "`GITHUB_TOKEN`" + ` is missing, STOP and ask the operator to run ` + "`agentry service bind github`" + ` rather than cloning a stranger's repo or hand-rolling fake creds.

AUTOMATION — when the user wants a scheduled job (cron, "every morning", "hourly") or a webhook handler (Stripe/GitHub/Slack events), do NOT hand-roll a daemon, a cron container, or a custom logs page. Read ` + "`docs_read(\"skills/automation\")`" + `: scaffold from the baked template at ` + "`/opt/agentry/automation/template`" + ` (a ` + "`kind: nextjs`" + ` app using ` + "`@agentry/automation`" + `) — you write schedules in ` + "`automations/jobs.ts`" + ` and webhooks in ` + "`automations/hooks.ts`" + `; run history, payloads, Run-now and Replay come free at ` + "`/_agentry`" + `. Run history persists to a bound DB (postgres/mysql/mongo/redis) or is ephemeral in-memory — tell the user to ` + "`agentry service bind`" + ` one for durable history.

YOU LIVE IN THE SANDBOX. After sandbox_create succeeds, EVERY action — file write, command run, doc read, search — happens through agentry tools (file_*, command_*, docs_read, project_*). NEVER reach for a host-side shell tool to "look around"; the sandbox docs and code are inside the container, NOT on your machine. Telltale signs you're on the wrong filesystem and must STOP: paths starting with /System/, /Users/, OrbStack/, /var/lib/buildkit/, /private/var/, or anything outside /workspace, /etc/sandbox/, /tmp/sandbox/. If you see those, your last command went to the host — re-run it via command_run or, for docs, docs_read.

BOOTSTRAP:
  1. sandbox_create with a descriptive sandbox_id.
  2. docs_read("CONTRACT") — five invariants that apply to every project kind (ports, services, auth, platform boundaries). Then docs_read("README") — recipe router; pick a project kind from it.
  3. EVERY app runs as a managed project. Call project_create with one of: nextjs, static-html, streamlit, fastapi, python-script, custom. NEVER write .sandbox-project.json by hand. NEVER tell the user to run a server themselves ("python3 -m http.server", "npm run dev", "streamlit run …"). NEVER use command_start for the user-facing process. The project manager owns lifecycle, ports, restarts, logs.
  4. ANY visual surface — HTML, JSX, CSS — STARTS with docs_read("skills/frontend-design") + docs_read("skills/theme-factory/themes/<theme>") (themes: arctic-frost, botanical-garden, desert-rose, forest-canopy, golden-hour, midnight-galaxy, modern-minimalist, ocean-depths, sunset-boulevard, tech-innovation). This rule applies REGARDLESS of project kind. Skipping it produces generic-looking output the user will ask you to redo.

CODE LIVES IN THE SANDBOX. Every change is file_write into /workspace/projects/<name>/. Never paste source as "the deliverable"; never write to the user's local working directory; never fall back to chat-only output. If you can't create a sandbox, STOP and tell the user what blocked you.

DONE = SHARE INSTRUCTIONS. You cannot create shares or deployments — those are operator actions in the dashboard. When the build works (project_list shows it running with a port), END by telling the user: open the sandbox page in the agentry dashboard and click Share (dev preview URL) or Deploy (durable prod URL). The port project_list reports is the right one — never tell the user to pick a different port.

PLATFORM PROBLEM → REPORT, DON'T PATCH. /auth/* pages, port binding, preview/deploy URLs, platform cookies, and /etc/sandbox or /var/run/agentry paths belong to agentry, not your app. If one misbehaves: capture project_logs evidence, tell the user it's platform-side, STOP. Never write workaround routes or hand-edit manifest ports to dodge a platform bug.

COMMUNICATION — one sentence before each tool call. Brief updates at load-bearing moments only. End-of-turn: 1-2 sentences on what changed + what to verify. No PLAN.md / NOTES.md unless asked.`

// NewServer builds an MCP server with every agentry tool registered
// against the given Client. The server is ready to be Run on a transport.
func NewServer(c *Client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "agentry",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	Register(srv, c)
	return srv
}

// RunStdio runs the server over the MCP stdio transport, blocking until
// the peer disconnects or ctx is canceled. Suitable as the body of main().
func RunStdio(ctx context.Context, c *Client) error {
	srv := NewServer(c)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
