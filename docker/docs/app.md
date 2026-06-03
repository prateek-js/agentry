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
   you have to start over. A page rendered in a Claude.ai artifact
   does NOT substitute for a managed project.

### Quoting `npm install` flags

`npm install foo@>=2` runs in bash, which sees `>=2` as output
redirection and creates an empty file named `=2` in the current
directory. ALWAYS quote pinned versions OR write `package.json`
first via `file_write` and `npm install` (no args, reads
package.json). Same applies to any shell command with `<`, `>`, `|`,
`&`, `;` in the argument.

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
- Auth (if needed): `lucia` (lightweight) or `next-auth` (more turnkey)

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
  "start_command": ["npm", "run", "dev", "--", "--hostname", "0.0.0.0", "--port", "3000"],
  "auto_restart": true,
  "env": { "NODE_ENV": "development" },
  "health_check": { "port": 3000, "path": "/" }
}
```

## Services — what's available + how to wire them in

Tell the user what they need to bind BEFORE writing data-access
code. Then write code that reads env vars; never hardcode connection
strings or API keys.

| Service     | Env var(s)                                | Node SDK                 |
|-------------|-------------------------------------------|--------------------------|
| postgres    | `DATABASE_URL`                            | `postgres` or `drizzle`  |
| mysql       | `DATABASE_URL`                            | `mysql2`                 |
| mongodb     | `MONGODB_URL`                             | `mongodb`                |
| redis       | `REDIS_URL`                               | `ioredis`                |
| clickhouse  | `CLICKHOUSE_URL` etc.                     | `@clickhouse/client`     |
| aws-s3      | `AWS_ACCESS_KEY_ID` etc.                  | `@aws-sdk/client-s3`     |
| smtp        | `SMTP_HOST`, `SMTP_PORT` etc.             | `nodemailer`             |
| stripe      | `STRIPE_SECRET_KEY`                       | `stripe`                 |
| openai      | `OPENAI_API_KEY`                          | `openai`                 |
| anthropic   | `ANTHROPIC_API_KEY`                       | `@anthropic-ai/sdk`      |

Pattern, ALWAYS:
1. `service_list` to confirm what the cluster has bindable.
2. `service_bind(sandbox_id=..., service="postgres")` with the user's
   real credentials.
3. Read env vars in your code (`process.env.DATABASE_URL!`). Never
   hardcode. Never inline secrets.
4. Start the project (`project_start`) AFTER the bind so the shell
   shim picks up the env on launch.

## Sharing one service across many apps — namespace OR clobber

The user binds ONE mongo / postgres / redis / s3 per cluster. Every
sandbox AND every deployed app inherits the SAME credentials, which
means the SAME data store. Two apps that both write to a collection
called `users` will overwrite each other.

**Rule: every app namespaces its writes under its own name. Never use
default db names, default schemas, or bare collection / key names.**

The platform stamps `AGENTRY_APP_NAME` into every app's env — the
deployment slug in prod, the sandbox id in dev. Read it; don't
override it. The fallback `?? "dev"` exists so a brand-new sandbox
boots without crashing, NOT so apps share a "dev" namespace by
accident.

### MongoDB

DON'T:

```ts
const db = client.db();              // empty → "test" DB or worse
const db = client.db("appdb");       // shared across every app
```

DO:

```ts
const dbName = process.env.AGENTRY_APP_NAME ?? "dev";
const db = client.db(dbName);
// optional: assert at boot that we got a real name
if (process.env.NODE_ENV === "production" && dbName === "dev") {
  throw new Error("AGENTRY_APP_NAME must be set in production");
}
```

### Postgres / MySQL

Use a per-app schema. Create it at boot, then set `search_path`:

```ts
const ns = process.env.AGENTRY_APP_NAME ?? "dev";
// Run once at boot. CREATE IF NOT EXISTS is idempotent + safe to
// re-run, so you don't need a separate migration step for it.
await sql`CREATE SCHEMA IF NOT EXISTS ${sql(ns)}`;
await sql`SET search_path TO ${sql(ns)}, public`;
// Every CREATE TABLE / SELECT now lands inside the per-app schema.
```

If you can't make schemas work (some managed Postgres tiers restrict
DDL), prefix table names instead: `${ns}_users`, `${ns}_sessions`.

### Redis

Prefix every key with the app namespace. A tiny helper keeps it
unmissable at the call site:

```ts
const ns = process.env.AGENTRY_APP_NAME ?? "dev";
const k = (suffix: string) => `${ns}:${suffix}`;

await redis.set(k(`session:${id}`), token);
await redis.zadd(k("leaderboard"), score, userId);
```

### S3 / object storage

Every object key gets the app prefix; the bucket is shared:

```ts
const prefix = process.env.AGENTRY_APP_NAME ?? "dev";
await s3.send(new PutObjectCommand({
  Bucket: process.env.AWS_S3_BUCKET!,
  Key: `${prefix}/uploads/${file}`,
  Body,
}));
```

When listing, scope to the prefix:

```ts
const out = await s3.send(new ListObjectsV2Command({
  Bucket, Prefix: `${prefix}/`,
}));
```

### ClickHouse

Same idea as Mongo: pick the DATABASE explicitly.

```ts
const database = process.env.AGENTRY_APP_NAME ?? "dev";
const client = createClient({ url: process.env.CLICKHOUSE_URL!, database });
// CREATE DATABASE IF NOT EXISTS at boot, then create tables inside it.
```

### Exception: services that already namespace by API key

Stripe, OpenAI, Anthropic, SMTP — the user's KEY is the namespace.
Two apps sharing the same Stripe key share the same customer ledger,
so apps using these services should ask the user for a SEPARATE key
per app rather than trying to multiplex one key across many apps.

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

6. **Update the manifest** — confirm `.sandbox-project.json`'s
   `start_command` and `health_check.port` match.

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
      - Port 3000 in use → kill the stray process; don't pick a
        different port (the deploy pipeline expects 3000).
      Then `project_restart` and go back to step (a).

   e. Stop the loop when (a)+(b)+(c) all pass: row `running healthy`,
      logs show "Ready in Xms" / "✓ Compiled successfully" / no
      errors, curl returns 200 with HTML. ONLY THEN move to step 9.

9. Tell the user how to access the app. Per the access-from-browser
   recipe in the parent server-instructions block, hand them either:
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

## Running behind the bridge — the rules apps trip on

Whether the user opens your app via a **Share link** (preview) or
**Deploy** (durable URL), the app is reachable through the agentry
bridge at `https://<name>.agentry.run`. The bridge terminates TLS at
the edge and forwards plain HTTP to your container. That introduces
four classes of bugs apps written for "I'll just run it on localhost"
have. Bake these in from day one or you'll spend an hour debugging
why "the cookies don't stick" or "the OAuth redirect goes to
localhost:3000".

### 1. Bind to 0.0.0.0, not localhost.

The runtime can't see a server bound to `127.0.0.1`. The default
manifest already does `--hostname 0.0.0.0`. Don't change it. Same for
`next start` in the production image — pass `-H 0.0.0.0`. Same for
any other server you start (express, fastify, gunicorn, …).

### 2. Trust the forwarded headers.

The browser sees `https://your-app.agentry.run`. Your app sees
`http://0.0.0.0:3000`. To bridge that gap the bridge stamps:

- `X-Forwarded-Proto: https`
- `X-Forwarded-Host: your-app.agentry.run`
- `X-Forwarded-For: <client ip>`

Read these — never `req.host` / `req.protocol` directly — whenever
you need to know the public URL of THIS request.

**Next.js (App Router):**

```ts
// src/lib/url.ts
import { headers } from "next/headers";

export async function publicBaseURL(): Promise<string> {
  const h = await headers();
  const host = h.get("x-forwarded-host") ?? h.get("host") ?? "localhost:3000";
  const proto = h.get("x-forwarded-proto") ?? "http";
  return `${proto}://${host}`;
}
```

Use it any time you build an absolute URL: OAuth callbacks, email
links, social-share preview cards, payment-provider return URLs, sitemap
entries, Open Graph tags. NEVER paste `http://localhost:3000` into
generated HTML or emails.

**Express / fastify**: `app.set("trust proxy", 1)` once at boot;
then `req.protocol`, `req.hostname` reflect the forwarded values.

### 3. Cookies — Secure, SameSite, no Domain.

The browser sees the app over HTTPS, so:

- `Secure: true` is REQUIRED on session/auth cookies. Without it the
  browser drops them on the first navigation.
- `SameSite: "lax"` is the right default — works with normal
  navigation and OAuth redirects. `"strict"` breaks OAuth callbacks
  (the redirect counts as cross-site). `"none"` requires Secure
  anyway, and you only need it for cross-origin iframes (rare).
- `HttpOnly: true` for anything sensitive (session id, auth token).
- DO NOT set a `Domain` attribute. Let the browser scope the cookie
  to `<name>.agentry.run`. Setting `Domain=.agentry.run` is illegal
  cross-tenant and will be rejected; setting `Domain=localhost` won't
  match the deploy URL.

Next.js example:

```ts
import { cookies } from "next/headers";

(await cookies()).set("sid", sessionToken, {
  httpOnly: true,
  secure: true,
  sameSite: "lax",
  path: "/",
  maxAge: 60 * 60 * 24 * 7,
});
```

If you're using `next-auth` / Auth.js, set `AUTH_TRUST_HOST=true` in
the deploy env. Otherwise it'll refuse to set cookies because the
host isn't on its allow-list.

### 4. Don't hardcode the URL.

Common offenders:

- `NEXTAUTH_URL=http://localhost:3000` baked into `.env` →
  authentication redirects loop back to localhost in prod.
- Stripe `success_url: "http://localhost:3000/thanks"` → user lands
  on a dead page after checkout.
- OAuth provider's allowed-callback list pinned to localhost → prod
  rejects with `redirect_uri_mismatch`.

The fix: compute these from `publicBaseURL()` (or the equivalent in
your framework), and set the corresponding env var in the deploy
form when you can't compute it.

`process.env.NEXT_PUBLIC_APP_URL` is the conventional name we use —
set it in the deploy env editor to the public URL once the deploy
URL is known. Code reads `process.env.NEXT_PUBLIC_APP_URL ?? await
publicBaseURL()` and is correct in both modes.

### 5. WebSockets, streaming, long requests — all fine.

The bridge proxies WebSockets and chunked / streaming responses
unchanged. Server-Sent Events work. Long-polling works. You don't
need anything special; if a request would have worked on localhost,
it works through the bridge.

### Sanity check before telling the user "it's live"

After the deploy goes green:

```
curl -sI https://<name>.agentry.run/
```

Expect a 200 (or 3xx that lands on a 200). If you get a redirect to
`http://localhost:3000/...` — you violated rule #2 or #4 above; fix
the code, redeploy.

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
