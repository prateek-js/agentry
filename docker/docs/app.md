# Recipe — scaffold a Next.js app (single-process, one URL)

Use this when the user asks for "a dashboard", "a web app", "an
internal tool", "a UI on top of …", or any variant involving a browser
UI backed by an API. The default stack is **Next.js (App Router, TS)**
with the API + UI in the SAME project — one process, one port, one
deploy.

Don't split frontend from backend. Don't reach for FastAPI + Vite, two
projects, Procfiles, or Docker Compose. agentry apps ship as a single
image with a single URL. If you find yourself making `projects/backend`
and `projects/frontend`, stop — you're on the wrong path.

**Prerequisite: `docs_read("CONTRACT")` — the five invariants
(ports, one-project, services, auth, platform boundaries). This
recipe assumes them.**

## ⚠️ READ THIS FIRST — scaffold BEFORE you explore

The #1 way this recipe goes wrong is: the model spawns a Jupyter
kernel to "look at the data first", writes analysis inline, drafts a
React component as a one-off, and never produces a managed Next.js
project. At the end there's no `/workspace/projects/app/`, no
`.sandbox-project.json` — just a kernel and some PNG charts. The user
can't open it in a browser, ship it, or deploy it.

DO NOT DO THAT. Rules:

1. The **FIRST `file_write` in this task** is the project skeleton.
   Before any `code_exec`, before any `npm install`, before any
   feature code, you write:
     - `/workspace/projects/app/.sandbox-project.json`
     - `/workspace/projects/app/package.json`
     - `/workspace/projects/app/next.config.mjs`
     - `/workspace/projects/app/tsconfig.json`
     - `/workspace/projects/app/src/app/layout.tsx` (placeholder)
     - `/workspace/projects/app/src/app/page.tsx` (placeholder)
   This commits you to the app being ONE managed project.
2. Use `code_exec` for **ad-hoc exploration ONLY** — sample queries, a
   schema probe, a quick histogram. ≤30 lines per call. Never put
   business logic, routes, or React components in the kernel.
3. The moment you're writing a real route, a server action, a UI
   component, or a data-access function: `file_write` it into the
   matching folder. The kernel is a scratch buffer, not your codebase.
4. The app isn't "built" until `project_list` reports the project
   `running` with port `3000` discovered. Empty `project_list` means
   you have to start over. A page rendered as a chat-embedded artifact
   does NOT substitute for a managed project.

### Quoting `npm install` flags

`npm install foo@>=2` runs in bash, which sees `>=2` as output
redirection and creates an empty file named `=2` in the current
directory. ALWAYS quote pinned versions OR write `package.json`
first via `file_write` and `npm install` (no args, reads
package.json). Same applies to any shell command with `<`, `>`, `|`,
`&`, `;` in the argument.

## ⚠️ BEFORE YOU SAY THE TASK IS DONE — run the real build

`next dev` runs your code through Turbopack with relaxed type
checking; `next build` runs the full TypeScript compile. Code that
loads and renders happily in dev can fail to build with errors like
"Cannot find name 'ProductDoc'" (missing import), "Type '…' is not
assignable" (drift between caller and callee), or "Property '…' does
not exist on type" (shape mismatch). These never surface from
`port_wait` or curling the dev server — they only surface when
something runs a real build.

So run it yourself, as the LAST step before you tell the user the
task is complete:

```
project_stop name=app        # next build and next dev fight over .next/
command_run "cd /workspace/projects/app && npm run build" (timeout 300)
project_start name=app
```

- exit 0 → safe to report done.
- non-zero → read the compiler error verbatim, fix the reported
  files via `file_write` / `file_replace`, run the build again.
  Repeat until it exits 0, then `project_start`.

A missing import or stale type is a 60-second fix while you're still
in the conversation. The same error caught only when the user later
tries to ship the project costs them a round trip back to chat.

## Port discipline — CONTRACT.md rule 1

**Never pass `--port`, `-p`, or `--hostname` in `start_command`.**
Use the scaffolded `["npm", "run", "dev"]` and let `next dev` honor
the `PORT` env var the runtime sets. Full rationale in
`docs_read("CONTRACT")` — the short version: when agentry auth is
on, a sidecar owns the public port and your app gets `PORT=3001`;
a hard-coded port collides and dies with `address already in use`.

When auth is on, login UI is served by the platform at `/auth/login`
(alias `/auth/signin`); your code reads `x-forwarded-email` /
`-user` / `-name` / `-provider` headers. See `skills/auth/SKILL.md`.

## Layout — one managed Next.js project

```
/workspace/
└── projects/
    └── app/
        ├── .sandbox-project.json     # type: "app"
        ├── package.json
        ├── next.config.mjs
        ├── tsconfig.json
        ├── .gitignore
        └── src/
            ├── app/
            │   ├── layout.tsx        # root layout (~30 lines)
            │   ├── page.tsx          # / route (~50 lines)
            │   ├── globals.css       # tailwind imports or plain CSS
            │   ├── <feature>/
            │   │   └── page.tsx      # /<feature> route
            │   └── api/
            │       └── <feature>/
            │           └── route.ts  # GET/POST/... handlers
            ├── components/
            │   └── <Thing>.tsx       # one file per component, ~80 lines max
            ├── lib/
            │   ├── db.ts             # DB client (drizzle / postgres.js / …)
            │   ├── <feature>/
            │   │   ├── queries.ts    # DB / business logic
            │   │   ├── schema.ts     # zod or drizzle schemas
            │   │   └── queries.test.ts
            │   └── format.ts         # tiny shared utilities
            └── public/
                └── favicon.svg
```

Every file stays under ~100 lines. Feature-folder layout, not
layer-folder. See `/etc/sandbox/docs/coding-style.md`.

App Router note: `src/app/<feature>/page.tsx` is the page for
`/feature`. `src/app/api/<feature>/route.ts` exports `GET`, `POST`,
etc. handlers for `/api/feature`. Both live in the SAME project, get
built into the SAME image, and run as ONE process. No CORS, no proxy,
no fetching across services.

## Minimum viable — files to scaffold

### `package.json`

```json
{
  "name": "<app-name>",
  "private": true,
  "scripts": {
    "dev": "next dev",
    "build": "next build",
    "start": "next start"
  },
  "dependencies": {
    "next": "^15.0.0",
    "react": "^19.0.0",
    "react-dom": "^19.0.0"
  },
  "devDependencies": {
    "@types/node": "^22.0.0",
    "@types/react": "^19.0.0",
    "@types/react-dom": "^19.0.0",
    "typescript": "^5.6.0"
  }
}
```

Add libraries to `dependencies` only as you use them. Common picks:
- DB: `postgres` (postgres.js — small, fast, supports prepared statements) OR `drizzle-orm` (typed query builder + migrations)
- Validation: `zod`
- Forms: react-hook-form + zod
- Tailwind: `tailwindcss postcss autoprefixer` + Tailwind config
- Auth: **CHECK env first**. If `AGENTRY_AUTH_ENABLED=true` is in env (`command_run "env | grep AGENTRY_AUTH"`), the authproxy sidecar handles login/signup/OAuth — read `skills/auth/SKILL.md` before touching auth code; do NOT install `next-auth`, `lucia`, or `better-auth`. If auth is NOT enabled and the user explicitly wants login, then `lucia` or `next-auth` are reasonable picks.

Don't pull in a UI kit (shadcn, MUI) unless the user asks — vanilla CSS or Tailwind covers most demos.

### `next.config.mjs`

```js
/** @type {import('next').NextConfig} */
const nextConfig = {
  // Standalone output ships a self-contained server we can run
  // without node_modules — keeps the deploy image small.
  output: 'standalone',
};
export default nextConfig;
```

### `tsconfig.json`

Use the one Next.js scaffolds — it generates correctly on first
`next dev` if absent. Acceptable starter:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["dom", "dom.iterable", "esnext"],
    "allowJs": true,
    "skipLibCheck": true,
    "strict": true,
    "noEmit": true,
    "esModuleInterop": true,
    "module": "esnext",
    "moduleResolution": "bundler",
    "resolveJsonModule": true,
    "isolatedModules": true,
    "jsx": "preserve",
    "incremental": true,
    "plugins": [{ "name": "next" }],
    "paths": { "@/*": ["./src/*"] }
  },
  "include": ["next-env.d.ts", "**/*.ts", "**/*.tsx", ".next/types/**/*.ts"],
  "exclude": ["node_modules"]
}
```

### `src/app/layout.tsx`

```tsx
import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "<app-name>",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
```

### `src/app/page.tsx` (Server Component — common case)

```tsx
// Default: Server Components. Fetch data directly here — no /api hop,
// no useEffect, no loading state, the server renders with data.
import { listItems } from "@/lib/items/queries";

export default async function HomePage() {
  const items = await listItems();
  return (
    <main>
      <h1>Items</h1>
      <ul>
        {items.map((it) => (
          <li key={it.id}>{it.name} — qty {it.qty}</li>
        ))}
      </ul>
    </main>
  );
}
```

### `src/app/api/items/route.ts` (only when something OUTSIDE this app needs to hit it)

```ts
import { NextResponse } from "next/server";
import { listItems } from "@/lib/items/queries";

export async function GET() {
  const items = await listItems();
  return NextResponse.json(items);
}
```

> Most pages should NOT call `/api/*` from the browser. Server
> Components fetch directly via `queries.ts`. Use `/api/*` only when
> something other than this app needs the data — webhooks, an external
> caller, the LLM testing via curl.

### `src/lib/db.ts` — Postgres

```ts
// Reads DATABASE_URL — set by `agentry service bind postgres` or via
// the dashboard's Service catalog. Don't construct the URL by hand.
import postgres from "postgres";

declare global {
  // eslint-disable-next-line no-var
  var __pg: ReturnType<typeof postgres> | undefined;
}

// HMR-safe singleton: Next.js dev reloads modules, so the global
// guard stops us from leaking new pools on every save.
export const sql = global.__pg ?? postgres(process.env.DATABASE_URL!, {
  max: 5,
  idle_timeout: 30,
});
if (process.env.NODE_ENV !== "production") global.__pg = sql;
```

For other databases:
- **MongoDB**: `import { MongoClient } from "mongodb"` reading `MONGODB_URL`
- **Redis**: `import Redis from "ioredis"` reading `REDIS_URL`
- **MySQL**: `import mysql from "mysql2/promise"` reading `DATABASE_URL`
- **ClickHouse**: `import { createClient } from "@clickhouse/client"` reading `CLICKHOUSE_URL`

### `src/lib/items/queries.ts` — feature logic

```ts
import { sql } from "@/lib/db";

export type Item = {
  id: number;
  name: string;
  qty: number;
};

export async function listItems(limit = 50): Promise<Item[]> {
  return sql<Item[]>`
    SELECT id, name, qty FROM items ORDER BY id DESC LIMIT ${limit}
  `;
}
```

### `.sandbox-project.json`

```json
{
  "name": "app",
  "type": "app",
  "start_command": ["npm", "run", "dev"],
  "auto_restart": true,
  "env": { "NODE_ENV": "development" },
  "health_check": { "path": "/" }
}
```

## Services + data namespacing — read services.md

`docs_read("services")` for the binding table (which env vars each
service exposes), the ALWAYS-pattern (`service_list` → `service_bind`
→ read env → `project_start`), and the per-service namespacing
recipes (`AGENTRY_APP_NAME` as mongo db / postgres schema / redis
key prefix / s3 key prefix). Every app that touches a shared service
MUST namespace its writes — two apps writing a bare `users`
collection clobber each other.

## Recipe — end-to-end

Do these in order. **Don't skip step 0.**

0. **SCAFFOLD FIRST (before any exploration, install, or feature
   code).** `file_write` the project manifest, `package.json`,
   `next.config.mjs`, `tsconfig.json`, and placeholder
   `src/app/layout.tsx` + `src/app/page.tsx`. After this step:

   ```
   command_run "ls -R /workspace/projects/app"
   ```

   should show the project tree. The app exists as a shape. Now you
   can explore safely.

1. **Confirm the data shape.** If the app reads from an upstream
   (DB, API, file dump, …), `code_exec` a quick schema probe. <30
   lines per call, throwaway only. The moment you want to keep a
   query, `file_write` it into `src/lib/<feature>/queries.ts`.

2. **Sketch the feature list.** 3-5 routes max for v1. Each becomes
   a folder under `src/app/<feature>/` plus matching
   `src/lib/<feature>/`.

3. **Bind services the user needs** (postgres, redis, openai, …)
   via `service_bind`. Confirm `service_list(secrets=true)` shows
   the env vars now present.

4. **Write the project, in order**:
   - `src/lib/db.ts` (or the right client for your upstream)
   - per-feature `src/lib/<feature>/queries.ts` + `schema.ts`
   - per-feature `src/app/<feature>/page.tsx` (Server Component
     calling queries directly)
   - `src/app/api/<feature>/route.ts` ONLY if something external
     needs to hit it
   - `src/app/page.tsx` and `src/app/layout.tsx` finalised

5. `command_run "cd /workspace/projects/app && npm install"` (takes
   ~30-60 s on a cold cache).

6. **Leave the manifest alone** — `project_create` scaffolded the
   right `start_command`. Don't add `--port`/`--hostname` flags and
   don't add a `health_check` block (if you do add one, its `port`
   field is required by the schema).

7. `project_start` — starts Next.js dev server with auto-restart.

8. **VERIFY-AND-FIX LOOP — don't skip.** The build isn't done when
   `project_start` returns; it's done when the dev server is actually
   serving healthy responses. Walk this loop AT LEAST ONCE every
   build, more if anything looks off:

   a. `project_list(sandbox_id=...)` — does the row show `running`,
      port `[3000]` discovered, `healthy`? Anything else means
      something's wrong; don't tell the user the app is up.

   b. `project_logs(sandbox_id=..., project="app", tail=200)` — read
      the LAST 200 LINES. You're looking for:
      - "Error:" / "TypeError:" / "ReferenceError:" / "SyntaxError:"
      - "Module not found" / "Cannot find module"
      - "EADDRINUSE" / "port already in use"
      - Stack traces (lines starting with "    at ")
      - "Failed to compile" / Next.js red error overlay text
      - Database errors ("ECONNREFUSED", "password authentication failed")
      - Missing env vars ("undefined" in template literals,
        "process.env.X is undefined")

   c. `curl -i http://localhost:3000/` via command_run — does it return
      200 with HTML? 500s mean an error you missed in the logs;
      hard-refresh by reading logs again.

   d. If you found a problem: FIX IT, don't just tell the user.
      Common fixes you should apply yourself:
      - "Module not found" → add to package.json + `npm install <pkg>`
      - "Cannot find module @/lib/x" → write the file, don't just
        delete the import
      - Compile errors → re-read the file with file_read,
        file_write the fix
      - DB connection refused → did you bind the service? service_bind
        first, then project_restart.
      - Port in use → FIRST check whether the holder is the agentry
        auth sidecar (`authproxy` in `ss -tlnp` output). If it is,
        that's the platform working as designed — your app must read
        $PORT instead of hard-coding (CONTRACT.md rule 1). Only kill
        processes YOU started; never pick a different port (the
        deploy pipeline expects the standard one).
      Then `project_restart` and go back to step (a).

   e. Stop the loop when (a)+(b)+(c) all pass: row `running healthy`,
      logs show "Ready in Xms" / "✓ Compiled successfully" / no
      errors, curl returns 200 with HTML. ONLY THEN move to step 9.

9. Tell the user how to access the app (the DONE protocol). Hand
   them either:
   - "Click Share in the dashboard's Shared ports panel on port 3000"
     for a quick preview link, or
   - "Click Deploy to ship a prod build with a durable URL."

   Don't construct URLs yourself. Don't paste internal hostnames.
   Don't keep the chat going to "test" — that's the user's job. Stop.

### The exit check

Before you tell the user "the app is up," run:

```
command_run "ls -R /workspace/projects/app/src"
command_run "wc -l /workspace/projects/app/src/**/*.{ts,tsx}"
project_list(sandbox_url=...)
```

You should see: the project tree on disk, every file under ~100
lines, the project running with port 3000 discovered. If any of
those three checks fail, you haven't finished — keep going.

## Patterns to lean on

- **Server Components by default** — page.tsx is `async`, calls
  queries directly, server-renders with data. No useEffect, no
  loading flicker, no /api hop.
- **Server Actions for mutations** — `"use server"` on a function
  in `actions.ts`; form `action={createItem}` posts directly.
- **`/api/*` ONLY for external callers** — webhooks, integration
  endpoints, the LLM hitting it with curl. Your own pages don't.
- **`zod` validates everything from outside** — form input, request
  body, query string. Parse it; don't trust it.
- **One file, one component** — `page.tsx` for the route; everything
  else in `components/`. Hard cap ~100 lines.

## Running behind the bridge — read bridge.md

`docs_read("bridge")` for the four URL/cookie classes of bugs every
app hits behind the preview/deploy proxy: bind 0.0.0.0, trust
`x-forwarded-*` headers for absolute URLs, cookie attributes
(Secure + SameSite=Lax + NO Domain), and never hardcoding
localhost URLs. Plus the `curl -sI` sanity check before declaring
the app live. Applies to every project kind, not just Next.js.

## Common pitfalls

- **Don't split into projects/backend + projects/frontend.** The
  whole point of Next.js here is that the API and UI live in the
  same project. If you find yourself wanting a separate FastAPI
  service, ask the user — most of the time the answer is "no, just
  add a route handler in the existing app."
- **Don't fetch your own /api/* from a page.** Server Components
  call the underlying function directly. `/api/*` is for outside
  consumers.
- **Don't `npm install` while `next dev` is running.** Stop the
  project first (`project_stop`), install, then `project_start`.
  HMR breaks weirdly otherwise.
- **Don't open a fresh client per request.** Module-scoped
  singletons (with the HMR guard above) — pooling is the driver's
  job, opening is yours.
- **Don't hardcode connection strings or API keys.** They come from
  env vars set by `service_bind`. If you find yourself typing
  `postgres://`, you're doing it wrong.
- **Don't reach for a state-management library on day 1.**
  `useState` + `useReducer` cover almost every dashboard. Redux /
  Zustand / Jotai only when there's a real ask.
- **Don't write CSS-in-JS unless asked.** Tailwind or a plain
  `globals.css` is plenty for v1.
