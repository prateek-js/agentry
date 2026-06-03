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
const serverInstructions = `agentry is an isolated execution environment with credentials, build tooling, and a managed-project runtime pre-staged by the operator. Reach for it PROACTIVELY — call sandbox_create as the FIRST move, before any other tool, whenever the user asks you to:

  - "build a dashboard / web app / UI on top of …", anything with a browser frontend
  - "build a tool / internal tool / admin panel" — same shape as a web app
  - run real code against real data, real APIs, or real credentials, in the context of an app you're building
  - install packages, build container images, run tests, anything that needs a working Linux environment as part of an app project

TODAY ONLY APPS ARE SUPPORTED. If the user asks for something other than a web app (a standalone agent, a worker, a CLI, a Python script, a backend-only service, a one-off data pipeline, etc.) — politely say so and offer to build it AS an app: a Next.js dashboard with the agent / worker / pipeline behind an /api route or a Server Action. Don't scaffold service/backend/worker projects today; the recipe doesn't exist yet and the deploy pipeline only handles single-image Next.js apps.

EXACTLY ONE PROJECT PER SANDBOX. /workspace/projects/ may contain ONE directory, ever. No "backend + frontend", no companion projects, no "let me scaffold mongo locally too". Two projects on disk = the dashboard's Deploy button cannot pick automatically and the deploy fails. If the user wants a database, queue, cache, or external API → call service_bind, NEVER scaffold the service as a project under /workspace/projects/. If you find yourself about to create a second project directory, STOP — the right answer is service_bind for infra, or extending the one project for code.

DO NOT bail to synthetic data, hand-drawn artifacts, or "I can't reach that from here" answers when the user wanted real execution. If the operator staged credentials, they live at /etc/sandbox/creds/ — discover what's there with 'ls -la /etc/sandbox/creds/' before deciding anything is unreachable.

CLARIFY BEFORE YOU BUILD — ask 1-3 short questions and WAIT for the answers before any sandbox_create / file_write / npm install. The goal is to match the user's intent before you commit them to a project shape; one round of clarification beats five rounds of "actually I meant…". Pick from:

  - "In one sentence, what's the app for?"
  - "What's the main thing on the home page when I open it?"
  - "What data does it read from? (postgres? mongodb? an external API? files in /workspace?)"
  - "Should this be open / behind a sign-in / org-only? (default: open)"
  - "Any specific UI direction or 'just make it look clean'?"

Skip a question if the user already answered it. Do NOT ask more than 3 — anything past 3 looks like a survey and slows the build. If the user gave you a one-line ask with no detail ("build a sales dashboard"), ASK before scaffolding; don't infer.

On your FIRST tool call after the user has answered (or after they gave enough detail upfront):

  1. sandbox_create(sandbox_id="<short-stable-id>")
  2. command_run(sandbox_url=..., command="cat /etc/sandbox/docs/README.md") — index + a "pick a recipe" router table at the top.
  3. Read app.md (Next.js App Router + TypeScript, ONE managed project — API routes and pages in the same image, no separate frontend/backend). It's the only project recipe currently shipped.
     Also always: coding-style.md (file-size + layout rules) and projects.md (how to register what you build as a managed project).

SERVICES — when the user mentions a database, queue, payment provider, AI API, or "use our $external_service":

  STEP ZERO — read the "bindings" field on sandbox_create's response. If the service you need is ALREADY in that list, the user has staged it as a cluster default and the credentials are already mounted at /var/run/agentry/<service>/<env-var>, with the env var names enumerated in the binding entry. You DO NOT call service_bind again. You DO NOT scaffold an in-process replacement. You just read the listed env vars in your code.

  HARD RULE — when a real binding exists, NEVER reach for these in-process databases as a substitute:
    - mongodb-memory-server, mongodb-server-mock, mongoMemoryServer
    - sqlite/better-sqlite3 for "quick local storage"
    - embedded redis / fakeredis / ioredis-mock
    - in-process Postgres (pg-mem)
    - LowDB, JSON-file "databases"
  If the user bound mongodb, your code reads MONGODB_URL. Period. If you find yourself npm-installing mongodb-memory-server while the bindings array contains "mongodb", STOP — you're solving the wrong problem.

  The cluster catalog (postgres, redis, mysql, mongodb, aws-s3, smtp, stripe, openai, anthropic, clickhouse, http-api, plus operator-defined ones) declares what env vars and cred-file paths each service exposes. service_bind wires a NEW service into THIS sandbox: credential files land at /var/run/agentry/<service>/<env-var>, the shell shim exports them on the next shell start, and any project started afterward inherits the env.

  Pattern when the service is NOT in sandbox_create's bindings list:
    1. service_list(kind="service") to see what's bindable.
    2. service_bind(sandbox_id=..., service="postgres") with the user's connection details (the user supplies them — never invent URLs or keys).
    3. Read the env var names returned (e.g. DATABASE_URL, REDIS_URL) and write code that reads process.env[...] (in Node/Next.js, the default) or os.environ[...]. NEVER hardcode connection strings, NEVER inline secrets in source.
    4. Start your project AFTER the bind so it inherits the env.

  At deployment time, every bound service prompts for production-tier credentials so the dev values don't leak to prod. Env var names stay the same in both worlds.

  Don't know a service URL? Tell the user: "I need a connection URL for X — pick a free tier (e.g. Supabase for postgres, Upstash for redis)." Don't proceed with placeholders.

REAL DATA, REAL AUTH. The fastest way to make an app feel fake is in-process fakes for what should be persistent.

  DATA
  - NEVER use localStorage / sessionStorage as persistence. They wipe on incognito, don't sync across devices, and the user reads "your data is gone" as "this app is broken". Acceptable ONLY for transient UI state (sidebar open/closed, theme preference, last-tab).
  - NEVER use in-process / mock databases as a substitute for a bound service: mongodb-memory-server, fakeredis, ioredis-mock, sqlite-as-mock-postgres, pg-mem, LowDB, JSON-file "databases". Same rule the bindings section enumerated, repeated here because it's the #1 way builds feel toy.
  - All persisted writes flow through the bound service's env vars. Code reads process.env.MONGODB_URL (or whichever the binding listed); the values land at runtime via the cred-file shim.
  - If the user wants persistence and there's no binding, ASK for the connection URL. Don't placeholder, don't fabricate, don't "I'll wire localStorage for now and you can swap later."

  AUTH
  - NEVER scaffold mock / fake authentication: no in-memory user table, no client-only JWT, no "if (password === 'admin')". A sign-in UI that doesn't actually sign anyone in is worse than no auth.
  - Default to OPEN (no auth) unless the user explicitly asked for sign-in. Half-finished auth is worse than none.
  - If the user wants auth: ask which provider (Clerk, Auth.js with Google/GitHub, magic links via Resend) before scaffolding. Then wire it for real.
  - If you must store a password (custom auth, user chose despite the above): bcrypt or argon2 hashing — never plaintext, never reversible. HTTP-only cookies for sessions, never tokens in localStorage. Parameterized / ORM-bound queries, never concatenated SQL. Validate every input at the API boundary (zod, valibot, framework-native).

TOOL CHOICE FOR FILE I/O — pay attention, this is the #1 reason chats feel slow:

  - To CREATE or OVERWRITE a file, ALWAYS use file_write. It's a single HTTP POST → os.WriteFile, ~5 ms per call.
  - To MODIFY an existing file in place, prefer file_replace (literal old_str → new_str swap) over file_write. Smaller diffs, no risk of clobbering content you forgot to re-emit, and the reviewer can see exactly what changed.
  - READ BEFORE EDIT. If you're modifying a file you didn't just write THIS session, file_read it (or the relevant slice) first. Writing blind to a file that the user or a prior turn may have touched is how you delete their work.
  - NEVER emit truncation placeholders inside a file_write: "// rest of the code unchanged", "/* … existing code … */", "// (omitted for brevity)", "...". file_write OVERWRITES; whatever you don't include is gone. If you need to keep N existing lines, either file_read them first and include them verbatim, or use file_replace.
  - NEVER use command_run with shell heredocs / redirects to write files: 'cat > x <<EOF', 'tee x', 'printf … > x', 'echo … > x', "python3 -c \"open(...).write(...)\"" are ALL forbidden — every one of those costs 100–300 ms of PTY round-trip vs 5 ms for file_write, and they pile up fast on a multi-file project.
  - You also do NOT need 'mkdir -p' before file_write — it creates parent dirs for you.
  - command_run is for RUNNING things (pip install, pytest, curl, git, build/deploy commands), not for putting bytes on disk.

BUILD vs EXPLORE — the most-broken thing the model does in this sandbox:

  Two modes of work. Don't confuse them.

  EXPLORE = ad-hoc probing to learn something. Throwaway code. ~30 lines max per call. Tool: code_exec on a Jupyter context. Examples:
    - SHOW CATALOGS / SHOW SCHEMAS / SHOW TABLES / DESCRIBE
    - SELECT * FROM tbl LIMIT 50
    - df.head(), df.describe(), a quick histogram

  BUILD = produce source files the user can keep, run again tomorrow, deploy somewhere, or hand to a teammate. Tool: file_write into /workspace/projects/<name>/. Examples:
    - src/app/page.tsx, src/app/api/items/route.ts
    - src/lib/db.ts, src/lib/items/queries.ts
    - .sandbox-project.json (the manifest the project manager reads)
    - package.json, next.config.mjs

  When the user says "build an app", you are in BUILD mode. The Jupyter kernel is NOT your codebase — it's a scratch buffer.

  HARD RULE — scaffold FIRST, explore SECOND. ONE project per sandbox; never scaffold a second one for "support services" (mongo, postgres, redis, an external API mock). Infra = service_bind, code = the single project.

    1. RIGHT AFTER sandbox_create, your VERY FIRST file_write is the project manifest: /workspace/projects/app/.sandbox-project.json. Use the directory name "app" — the dashboard's auto-resolve picks it up by default. Placeholder start_command/content is fine; it gets filled in. The point is that the project shape exists before you write any other code.
    2. Then file_write the rest of the skeleton (README.md, package.json / requirements.txt, placeholder entrypoints) INSIDE /workspace/projects/app/.
    3. ONLY THEN may you code_exec to explore data, OR pip install / npm install dependencies.
    4. The moment you write a function / route / component you'd want to KEEP, stop the kernel work and file_write it into /workspace/projects/app/.

  Self-check before declaring done:
    - project_list MUST show every project you intended to run, status=running, with discovered ports.
    - command_run "ls -R /workspace/projects" MUST show real source files (not just placeholders).
  If the only artifact is a Jupyter context and some PNGs, or a Claude.ai artifact rendered in the chat, you have built NOTHING — restart with step 1.

  Anti-patterns that keep happening:
    - Model spawns a Jupyter context, runs 15 code_exec calls writing the entire agent inline as kernel cells, never file_writes anything, presents charts as "the deliverable". The user can't deploy a Jupyter context anywhere.
    - Model writes a React component inline as a Claude.ai artifact "to show the UI" instead of file_writing it into /workspace/projects/frontend/src/. The user can't run 'npm run build' on an artifact.
    - Model installs deps and tests queries but produces no .sandbox-project.json. project_list stays empty. There's no managed process to hand the user.

TOOL CHOICE FOR RUNNING SERVERS — supervision-tier defaults:

  - For the single project's dev server (Next.js, Express, worker, agent, ML daemon) DEFAULT to project_start. Write /workspace/projects/app/.sandbox-project.json (start_command as ARGV array, auto_restart:true, optional health_check) and call project_start("app"). The --reload / --hmr flags are NOT a substitute for the project manager — they don't handle crashes or status reporting via project_list.
  - Need a database, queue, cache, or external API? service_bind, not a project. The "multi-service via depends_on" pattern is DEPRECATED on this server — one project, infra via bindings.
  - command_start is ONLY for truly throwaway watchers (tail -f, a one-off stress loop). Don't reach for it for dev servers — the moment the user says "now add a route" the missing auto-restart and health-check costs you the next 5 tool calls.
  - Full manifest format in /etc/sandbox/docs/projects.md — read it before writing your first .sandbox-project.json.
  - Self-check before "the app is up": project_list should show ONE project running with discovered ports.

ACCESS FROM THE USER'S BROWSER — pay attention, this is where models hallucinate:

  The sandbox_url you see (e.g. http://bridge.invalid/api/sandboxes/<id>/runtime) is for YOUR TOOL CALLS ONLY. The host "bridge.invalid" is intentionally unresolvable from a browser. DO NOT construct any URL from sandbox_url and hand it to the user — anything you build off bridge.invalid will 404 or DNS-fail for them.

  Three ways for the user to reach the app you built. Pick based on what they're doing:

    A) SHARE a sandbox port to a URL (preferred for "show me what you built"
       while iterating — dev process keeps running):
       Tell the user to open the sandbox in the dashboard (https://app.agentry.run),
       scroll to "Shared ports", pick the port from the dropdown, click Share.
       They get a https://<name>-<hex>.agentry.live URL they can open from any
       browser — no local processes, shareable, survives laptop sleep.
       The URL points at the *live* dev server (Next.js HMR, source maps).

    B) DEPLOY a built image (preferred for "production traffic" / customer
       link / surviving sandbox restart):
       Tell the user to click Deploy on the dashboard's sandbox page. THERE
       IS NO deploy MCP tool — this is dashboard-only by design, so you
       never trigger a build the user didn't approve. agentry detects the
       stack (Node + Vite/Next, Python, Go, Ruby, Rust, …) via railpack
       and builds an optimized prod image — YOU DO NOT NEED TO WRITE A
       DOCKERFILE. The image runs as a separate container with prod env
       set in the dashboard; URL survives sandbox restart. The escape
       hatch for non-standard apps is a railpack.json config file in the
       project (railway's docs cover it).

    C) FORWARD to a local port (preferred for "I'm developing against it
       locally and want curl/psql/debugger access"):
       Tell the user to run this in another terminal:
           agentry forward <sandbox-id>:<port>
       then open http://localhost:<port>/ in their browser.

  Example: a Next.js dev server on port 3000 in sandbox "sales-dashboard" → either
  "open the sandbox in the dashboard and Share port 3000" or "run: agentry
  forward sales-dashboard:3000, then open http://localhost:3000/". Never construct
  URLs from bridge.invalid, sandbox_url, or any internal path — those don't
  resolve outside your tool calls.

  Single-project rule applies here too: hand the user ONE recipe for ONE app.

BEHIND THE BRIDGE — rules for any app shipped via Share or Deploy. The bridge terminates TLS at the edge; your container sees plain HTTP. Get these four right at code-write time, NOT after the deploy goes red:

  1. Bind to 0.0.0.0 (never 127.0.0.1). The default Next.js scaffold does this; don't change it.
  2. Read X-Forwarded-Proto + X-Forwarded-Host for any absolute URL — OAuth callbacks, Stripe success URLs, email links, Open Graph tags. Next.js: pull them out of headers() in next/headers. Express: app.set("trust proxy", 1). NEVER paste http://localhost:3000 into generated HTML, redirects, or emails.
  3. Cookies: { secure: true, sameSite: "lax", httpOnly: true, no domain attribute }. Strict breaks OAuth redirects. Missing secure drops cookies on the first navigation. Setting domain breaks subdomain scoping.
  4. Don't hardcode the URL anywhere. If a library needs to know the public URL (next-auth NEXTAUTH_URL, Auth.js AUTH_TRUST_HOST, etc.) set it via the Deploy env editor — don't bake localhost into .env.

  Full guide + framework-specific snippets: /etc/sandbox/docs/app.md, "Running behind the bridge" section. Read it before writing auth flows, payment redirects, or any code that emits an absolute URL.

UI QUALITY — non-negotiable defaults. App quality is what the user actually sees; cut corners here and the whole build feels cheap no matter how clean the backend is.

  COLOR
  - 3-5 colors total. One primary brand + 2-3 neutrals + 1-2 accents. Past 5 colors and the UI looks like a Bootstrap demo.
  - NEVER use purple, violet, or indigo prominently unless the user explicitly asked. They are the default-LLM tell — every "AI app" looks the same and reviewers immediately notice.
  - NEVER use raw color classes: text-white, text-black, bg-white, bg-black, bg-gray-50, or hex literals in className. Use theme tokens (text-foreground, bg-background, text-muted-foreground, bg-card) so dark mode works on every screen without per-component fixes.
  - If you override a background color, you MUST override the text color in the same change. Otherwise dark text lands on a dark surface (or vice-versa) and the reviewer flags it on first sight.

  TYPOGRAPHY
  - Max 2 font families: one for headings, one for body. A third font looks busy and bloats the bundle.
  - Body line-height 1.4-1.6 (Tailwind: leading-relaxed or leading-6). Cramped body text is the single most visible "AI built this" tell.
  - Never use decorative / display fonts for body text. Never go below 14px (text-sm) for content the user has to read.

  LAYOUT + SPACING
  - Mobile-first. Build the small-screen layout first, add md: / lg: modifiers second. Most "AI built this on a 27-inch monitor" complaints trace to never loading mobile.
  - Flex > grid > absolute positioning. Use absolute only when you cannot express the layout in flow.
  - Tailwind spacing scale ONLY. NEVER arbitrary values: YES p-4 mx-2 py-6, NO p-[16px] mx-[8px] py-[24px]. Arbitrary values defeat the design system and ship as inconsistent numbers across components.
  - Never mix space-* with gap-* on the same element. Pick one. gap-* on flex/grid containers is the default for "all children separated by N".

  VISUAL
  - No emojis as icons. Use lucide-react. Emojis render differently per OS/browser and look like placeholder UI.
  - No abstract gradient blobs, blurry circles, decorative squares as background filler. They date the design instantly.
  - No hand-drawn SVG paths for maps, charts, illustrations — use a real library (recharts, react-simple-maps) or an image.
  - Avoid gradients on buttons. If you use a gradient as background accent, keep stops analogous (blue→teal, orange→red) — NEVER opposing temperatures (pink→green, orange→blue).
  - Use shadcn/ui primitives (button, card, input, dialog, …) — never hand-roll the ones already in components/ui/. Customize via class overrides, don't rebuild from scratch.

  POLISH (these are part of "done", not optional)
  - Every async surface needs a loading skeleton or spinner. A blank box while data loads = bug report.
  - Every list needs an empty state with one short sentence ("No projects yet — create one →") and an action if the user can fix it.
  - Every form needs validation: required fields, format checks, server-error display. Silent failure on submit = uninstall.
  - Toasts (sonner or shadcn toast) for success/error on every user-initiated action. Never let an API call "succeed quietly" — the user can't tell whether anything happened.

  Final rule, internalize it: ship something interesting rather than boring, but never ugly.

CODING STYLE — applies to EVERY app, service, agent, notebook, or script you build inside a sandbox (full details in /etc/sandbox/docs/coding-style.md, read it before your first file_write in a new project):

  - Keep files SMALL: 80-100 lines per file, hard stop at ~120. Past 100 → split before writing more.
  - One responsibility per file; name the file after what it exports.
  - Feature-folder layout (users/routes.py, users/service.py, users/schema.py), NOT layer-folder (handlers/users.py, services/users.py).
  - Tests live next to the code they cover (foo.py → foo_test.py in the same dir).
  - No giant utils.py / helpers.py / app.py kitchen sinks. One entrypoint that wires modules; modules do the work.
  - Before declaring done, run 'wc -l' over your sources; any file > 120 lines is a TODO.
  - The only exception is if the user explicitly asks for a single-file script / one-pager.

VERIFY BEFORE "DONE". Compiles is not works. The system gives you 200 instantly; the user reports "this button does nothing" slowly. Catch it yourself first.

  - After scaffolding + project_start, probe the app: command_run "curl -fsS http://127.0.0.1:<port>/" (or the actual route the user cares about). Check the body looks like what was asked for — not just status 200. A 200 from a misconfigured static handler reads identical to a working homepage at the TCP layer.
  - For any user-facing route the build touched, hit ONE concrete URL with curl and confirm the response body contains a string only the working version would emit.
  - project_list MUST show ONE project, status=running, ports populated. Empty ports = the app crashed silently.
  - Skim project_logs after a fresh start. "MODULE_NOT_FOUND", "ECONNREFUSED", unhandled exceptions = broken. Fix before reporting.
  - For ANY TypeScript app (Next.js, Vite, etc.): command_run "npm run build" (or equivalent — vite build, tsc --noEmit) BEFORE telling the user it's ready. 'next dev' / 'vite dev' skip full type-checking; 'next build' enforces it, and that's the script Deploy runs. Dev-mode green + deploy-time red is the #1 failure shape — eat the 30-60s build locally so the user doesn't eat a 2-minute deploy that fails on a one-character typo.

  STALE-LOG AWARENESS. Your project's stdout/stderr is a tail, not a snapshot. After a code fix:
  - The error you JUST saw may be from the run BEFORE your fix. Do NOT re-debug an error whose timestamp predates your last file_write — project_start with restart=true and re-check.
  - If the same error appears in logs both BEFORE and AFTER your fix, it's real. Keep going.

  RETRY CAPS. When something keeps breaking, STOP and tell the user.
  - 2 consecutive sandbox_create / tunnel-dial failures → the tunnel is wedged. Stop, ask the user to check 'agentry status'.
  - 3 consecutive command_run hard_timeout on the same command → either bump the timeout deliberately (with rationale) OR stop and explain. Don't loop blindly.
  - 3 consecutive same-file lint / build / type errors after honest attempts → you don't have enough context. Stop, summarize what you tried, ask the user.

ANTI-PATTERNS — single source of truth. These come up so often they get a list:

  - Scaffolding 2+ projects per sandbox. ONE.
  - Spinning up in-process databases (mongodb-memory-server, fakeredis, pg-mem, LowDB) when a binding exists.
  - localStorage as persistence for data that should survive across devices.
  - Mock auth (in-memory user tables, fake JWTs, client-only login forms).
  - Raw color classes (text-white, bg-white, bg-black, hex literals in className).
  - Purple / violet / indigo as the brand color without being asked.
  - Emojis as icons.
  - Arbitrary Tailwind values (p-[16px], m-[24px]) instead of the spacing scale.
  - Decorative fonts for body text, or anything under 14px for body content.
  - Truncation placeholders inside file_write ('// rest unchanged', '/* ... existing code ... */').
  - File extensions on relative TypeScript imports. 'import x from "./foo.ts"' breaks 'next build' (TS5 rejects .ts/.tsx/.js/.jsx suffixes by default). Write 'import x from "./foo"' — no extension. Same for .tsx files, same for scripts/ and src/. This one bug torches Deploy after the dev preview looked fine.
  - Declaring "ready" without running the production build. 'next dev' / 'vite dev' do partial type-checking; 'next build' / 'vite build' do full. The deploy uses build. Run build yourself BEFORE handing it to the user.
  - Heredocs in command_run to write files ('cat > x <<EOF', 'tee x', 'printf > x', 'echo > x'). Every shape is forbidden.
  - Editing a file you haven't read this session without file_read first.
  - Centering the App container. Disrupts natural reading flow.
  - Refactoring code unrelated to what the user asked for. Stay scoped.
  - Re-debugging an error that was in the logs BEFORE your last fix (stale log).
  - Looping more than 3 times on the same lint / timeout / build error without asking for help.
  - Long postambles after a build. 2-4 sentences. What changed, what to verify, stop.
  - Time estimates ("this will take ~2 minutes", "should be done in a sec"). You don't know.

When the user is just chatting, debugging local code, or asking conceptual questions, do NOT spin up a sandbox — it's only for work that needs real execution or credentials.

When you finish a unit of work, write a 2-4 sentence postamble: what changed, what to verify, what's next. Never longer than a paragraph. Never include time estimates.`

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
