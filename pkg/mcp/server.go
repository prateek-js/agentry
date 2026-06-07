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

AUTH — call agentry_auth_setup when the user wants user accounts, sign-in, login, sessions, or password-protected pages. Do NOT hand-roll bcrypt, JWT, session cookies, OAuth, in-memory user tables, or localStorage sessions. Default to OPEN if auth wasn't asked. agentry_auth_setup scaffolds Better-Auth deterministically.

REAL DATA, NEVER FAKE. No localStorage as primary persistence. No in-process databases (mongodb-memory-server, fakeredis, pg-mem) as substitutes for a bound service. If the user wants persistence and there's no binding, ASK for the connection URL.

BOOTSTRAP:
  1. sandbox_create with a descriptive sandbox_id.
  2. command_run "cat /etc/sandbox/docs/README.md" — recipe router.
  3. Follow app.md (Next.js App Router); also read coding-style.md + projects.md. Visual direction: skills/frontend-design/SKILL.md + a theme from skills/theme-factory/.

CODE LIVES IN THE SANDBOX. Every change is file_write into /workspace/projects/<name>/. Never paste source as "the deliverable"; never write to the user's local working directory; never fall back to chat-only output. If you can't create a sandbox, STOP and tell the user what blocked you.

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
