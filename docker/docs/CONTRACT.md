# The agentry contract — five invariants that apply to EVERY project kind

Read this once per sandbox, before writing any code. Every recipe
assumes these. When a recipe and this file disagree, THIS FILE WINS.

## 1. Ports: bind `$PORT`, never hard-code

Your app reads the `PORT` env var and binds to it. NEVER pass
`--port N`, `-p N`, or `--server.port N` in `start_command` — use the
shell form so `$PORT` expands at launch:

```
["sh", "-c", "exec <your-server> --port \"${PORT:-<default>}\""]
```

(`project_create` scaffolds exactly this — don't undo it.)

Why: when the operator enables auth, the agentry sidecar binds the
public port (3000) and launches your app with `PORT=3001`. A
hard-coded port collides with the sidecar and dies with
`address already in use`. `project_list` reports only the PUBLIC
port — that one is always the right answer for sharing.

## 2. One sandbox = one project

`/workspace/projects/` contains exactly ONE directory. Databases,
caches, queues, third-party APIs are NOT second projects — they are
service bindings (rule 3). Never scaffold a second project; never
split frontend/backend into two projects (Next.js API routes or a
single FastAPI app cover it).

## 3. Bind services BEFORE writing code that needs them

Order is load-bearing:

1. `service_list` — see what this cluster offers
2. `service_bind` (or confirm it's already in the sandbox_create
   response's `bindings`/`env` arrays)
3. ONLY THEN write code that reads the env vars the bind reported

Code reads env (`DATABASE_URL`, `REDIS_URL`, `OPENAI_API_KEY`, …) —
never hard-coded connection strings, never in-process substitutes
(sqlite-as-postgres, fakeredis, mongodb-memory-server), never
localStorage as primary persistence. If the service isn't bindable
and the user wants it, ASK for the connection URL.

## 4. Auth belongs to the sidecar, not your app

If `AGENTRY_AUTH_ENABLED=true` is in the sandbox env, login/signup/
OAuth/sessions are handled by the agentry sidecar in front of your
app:

- It serves `/auth/login`, `/auth/signup`, `/auth/logout`, `/auth/me`.
- Your app reads `x-forwarded-user` / `-email` / `-name` / `-provider`
  headers — guaranteed present on every request that reaches you.
- DO NOT install next-auth / lucia / better-auth / passport /
  flask-login. DO NOT build login pages, user tables, or session
  cookies. Read `skills/auth/SKILL.md` before any auth-shaped work.

If it's NOT set and the user wants login: tell them to run
`agentry auth enable` on their CLI. Don't scaffold a substitute.

## 5. Platform problem → report it, DON'T patch around it

These surfaces belong to agentry, not your app: the `/auth/*` pages,
port binding behavior, the preview/deploy URLs, cookies set by the
platform, TLS, and anything under `/etc/sandbox/` or
`/var/run/agentry/`. If one of them misbehaves (403s on login, port
conflicts you didn't cause, a preview URL serving the wrong thing):

1. Capture the evidence (`project_logs`, the exact error)
2. Tell the user it's a platform-side issue, with the evidence
3. STOP. Do not write workaround routes, do not edit the manifest's
   ports by hand, do not "fix" platform cookies from app code.

A workaround hides the bug from the operator and breaks when the
platform is fixed. Reporting it gets it actually fixed.

---

When you're DONE building: tell the user to open the sandbox page in
the dashboard and click **Share** (dev preview URL) or **Deploy**
(durable prod URL). You cannot create shares or deployments yourself
— those are operator actions.
