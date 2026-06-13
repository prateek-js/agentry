# Skill: Auth (sandbox-side, sidecar-served)

Use when the user says something like "add login", "users need to
sign in", "I want auth", "gate this behind a user account". The
auth surface (login / signup / OAuth / sessions) is NOT something
you build. Read this whole page before touching auth code.

## How auth works on agentry

When the operator runs `agentry auth enable` on their CLI, every
sandbox in that profile gets:

1. A database binding (postgres, mysql, or mongodb). Reachable via
   `DATABASE_URL`, `POSTGRES_URL`, `MYSQL_URL`, or `MONGODB_URI`.
2. The `authproxy` sidecar baked into the runtime image. When
   `project_start` spawns your app, authproxy sits in front of it:

```
external --(:3000)-->  authproxy  --(127.0.0.1:3001)-->  your app
```

**Port discipline corollary (CONTRACT.md rule 1): because the
sidecar owns the public port, your `start_command` must NOT pass
`--port` / `-p` — your app reads the `PORT` env var (it will be
3001 when auth is on). A hard-coded port collides with the sidecar
and dies with `address already in use`. The scaffolded
start_command already does this; don't undo it.**

**If the auth surface itself misbehaves (403s on /auth/login, signup
loops, session not sticking): that is PLATFORM territory (CONTRACT
rule 5). Capture `project_logs`, report it to the user, and STOP. Do
not write warmup routes, do not edit ports, do not set competing
cookies from app code.**

3. The sidecar serves the entire auth surface itself:

   - `GET  /auth/login`       — login form
   - `POST /auth/login`       — credentials -> session cookie
   - `GET  /auth/signup`      — signup form
   - `POST /auth/signup`      — new user -> session cookie
   - `POST /auth/logout`      — clear session
   - `GET  /auth/me`          — JSON of the current user (or 401)
   - `GET  /auth/oauth/<provider>/start`   — OAuth flow start
   - `GET  /auth/oauth/<provider>/callback` — OAuth callback

   Configured providers (when the operator wired them):
   `google`, `github`, `microsoft`, `apple`, `generic-oidc`.

   When an `smtp` service is bound, password reset + email verification
   also light up automatically — the sidecar serves these and the login
   page grows a "Forgot password?" link. DO NOT build any of them:
   - `GET/POST /auth/forgot`  — request a reset link (enumeration-safe)
   - `GET/POST /auth/reset`   — set a new password from the emailed link
   - `GET      /auth/verify`  — confirm an email address

   The sidecar also enforces login rate-limiting + account lockout after
   repeated failed passwords — you don't need to add any of that either.

You don't render those pages, mount those routes, or write any of
that code. The sidecar already does.

## The signal: AGENTRY_AUTH_ENABLED

Check the env. If `AGENTRY_AUTH_ENABLED=true` is set, auth is wired.

```
command_run "env | grep AGENTRY_AUTH"
```

Possible outputs:

- `AGENTRY_AUTH_ENABLED=true` plus `AGENTRY_AUTH_DB=postgres` (or
  `mysql`) plus a `*_CLIENT_ID`/`*_CLIENT_SECRET` pair per enabled
  provider — auth is on. Proceed as below.
- Nothing — auth is NOT enabled. The sidecar is in passthrough mode
  and your app sees raw traffic. Build a public app. If the user
  asks for login, say:

  > Auth isn't enabled on this cluster yet. The operator runs
  > `agentry auth enable --db <postgres-binding>` from their CLI
  > to wire it. Until then I can't add a real login flow.

## When auth IS enabled: read the headers

For every authenticated request, the sidecar injects:

| Header                  | Meaning                          |
|-------------------------|----------------------------------|
| `X-Forwarded-User`      | Stable user id (32-char hex)     |
| `X-Forwarded-Email`     | Email (lowercased)               |
| `X-Forwarded-Name`      | Display name (may be empty)      |
| `X-Forwarded-Provider`  | `password` / `google` / …        |
| `X-Forwarded-Sig`       | HMAC over `uid|email|provider`   |

For unauthenticated requests the sidecar 302s the browser to
`/auth/login` BEFORE your handler runs, so by the time your code
sees a request these headers are guaranteed present.

The headers from incoming traffic are STRIPPED before injection —
a forged `X-Forwarded-User: admin` from the network cannot reach
your app. You can trust them.

### Optional: verify the signature

If your app is paranoid, verify `X-Forwarded-Sig` against
`AGENTRY_AUTH_SECRET`:

```python
import hmac, hashlib, os
SECRET = os.environ["AGENTRY_AUTH_SECRET"].encode()
def verify(req):
    uid   = req.headers["x-forwarded-user"]
    email = req.headers["x-forwarded-email"]
    prov  = req.headers["x-forwarded-provider"]
    want = hmac.new(SECRET, f"{uid}|{email}|{prov}".encode(),
                    hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, req.headers["x-forwarded-sig"])
```

```javascript
import crypto from "crypto";
const SECRET = process.env.AGENTRY_AUTH_SECRET;
function verify(req) {
  const { "x-forwarded-user": uid,
          "x-forwarded-email": email,
          "x-forwarded-provider": prov,
          "x-forwarded-sig": got } = req.headers;
  const want = crypto.createHmac("sha256", SECRET)
                     .update(`${uid}|${email}|${prov}`)
                     .digest("hex");
  return crypto.timingSafeEqual(Buffer.from(want), Buffer.from(got));
}
```

In practice most apps just trust the headers — the strip + sidecar
isolation is the security boundary, and the signature is
belt-and-braces.

### Reading the session from the browser

Your frontend can also hit `GET /auth/me` for JSON:

```json
{ "uid": "…", "email": "…", "name": "…", "provider": "password" }
```

Returns 401 with empty body when there's no session.

### Logout

A plain form POST to `/auth/logout` clears the session — no token
needed from your app (the sidecar's same-origin check is the guard).
Render a button inside `<form action="/auth/logout" method="POST">`;
the sidecar handles everything else.

## DO NOT add a second auth system

When auth is enabled by the operator, the sidecar IS the auth
system. Do not install:

- `next-auth` / `auth.js` / `NextAuth`
- `better-auth`
- `lucia`
- `passport`
- `flask-login`
- `django-allauth`
- any other login-form / session library

They will fight the sidecar for cookies and confuse the user.

Also do NOT:

- Build a `users` table yourself — the sidecar owns `agentry_users`
  in the bound DB.
- Render `/login` / `/signup` / `/forgot-password` pages — the
  sidecar serves `/auth/login` and `/auth/signup` already.
- Set `Set-Cookie` headers from your app — the sidecar sets the
  `agentry_session` cookie; touching it from your app desyncs the
  two.
- Pin `Domain=` on any cookie you DO set (for your own app state).
  The sidecar's cookies are exact-origin scoped on purpose; a
  cross-subdomain Domain on yours will surface confusing
  preview-vs-deploy state-leak bugs.

## DO NOT touch agentry_users from your app

The sidecar manages `agentry_users` (id, email, password_hash,
name, provider, provider_id, created_at). Read it if you need
to JOIN against user-id, but DO NOT write to it from your app —
the sidecar owns the schema and can change it across releases.

If you need extra per-user state, make your OWN table keyed by
`X-Forwarded-User` (the 32-char hex id is stable across sessions
and providers).

```sql
CREATE TABLE app_user_prefs (
  user_id      TEXT PRIMARY KEY,
  theme        TEXT NOT NULL DEFAULT 'system',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

## Quick worked example: Next.js

```tsx
// app/page.tsx — a server component reading the identity header.
import { headers } from "next/headers";

export default async function Home() {
  const h = await headers();
  const email = h.get("x-forwarded-email");
  const name  = h.get("x-forwarded-name") ?? email;
  return (
    <main>
      <h1>Hello, {name}</h1>
      <form action="/auth/logout" method="POST">
        <button>Sign out</button>
      </form>
    </main>
  );
}
```

Notice: NO `<SignInButton>`. NO `getServerSession()`. NO
`auth/[...nextauth]/route.ts`. Just headers.

## Quick worked example: FastAPI

```python
# app/main.py
from fastapi import FastAPI, Header
app = FastAPI()

@app.get("/")
def root(
    x_forwarded_email: str = Header(...),
    x_forwarded_name: str  = Header(default=""),
):
    name = x_forwarded_name or x_forwarded_email
    return {"hello": name}
```

## Operator runs the show

Auth wiring (enabling auth, choosing the DB, configuring providers)
is operator-side via the CLI:

```
agentry auth enable --db postgres
agentry auth providers add google
agentry auth providers add github
agentry auth status
```

You can mention this to the user if they ask about provider setup,
but DO NOT try to run those commands from inside the sandbox —
they're operator-side CLI commands, not sandbox tools.

## Summary

- Check env for `AGENTRY_AUTH_ENABLED=true`.
- If yes: read `X-Forwarded-Email` / `-User` / `-Name` / `-Provider`
  headers; render UI accordingly; don't build a login page.
- If no: build a public app, and tell the user the operator needs to
  run `agentry auth enable` if they want login.
