# Running behind the bridge — the rules apps trip on (EVERY kind)

Whether the user opens your app via a **Share link** (preview) or
**Deploy** (durable URL), the app is reachable through the agentry
bridge. The bridge terminates TLS at the edge and forwards plain
HTTP to your container. That introduces four classes of bugs apps
written for "I'll just run it on localhost" have. Bake these in from
day one or you'll spend an hour debugging why "the cookies don't
stick" or "the OAuth redirect goes to localhost:3000".

These rules apply to nextjs, fastapi, streamlit, static-html, and
custom alike.

### 1. Bind to 0.0.0.0, not localhost.

The runtime can't see a server bound to `127.0.0.1`. The scaffolded
manifests already bind 0.0.0.0 — don't change it. Same for any other
server you start (express, fastify, gunicorn, …).

### 2. Trust the forwarded headers.

The browser sees `https://your-app.agentry.live`. Your app sees
plain HTTP on an internal port. To bridge that gap the bridge
stamps:

- `X-Forwarded-Proto: https`
- `X-Forwarded-Host: your-app.agentry.live`
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
links, social-share preview cards, payment-provider return URLs,
sitemap entries, Open Graph tags. NEVER paste
`http://localhost:3000` into generated HTML or emails.

**Express / fastify**: `app.set("trust proxy", 1)` once at boot;
then `req.protocol`, `req.hostname` reflect the forwarded values.

**FastAPI / starlette**: pass
`--forwarded-allow-ips="*"` to uvicorn, or read
`request.headers["x-forwarded-host"]` directly.

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
  to the app's own hostname. Setting `Domain=.agentry.live` is
  illegal cross-tenant and will be rejected; `Domain=localhost`
  won't match the deploy URL.
- Reminder (CONTRACT.md rule 4): when agentry auth is enabled, the
  platform owns the session cookie — don't mint a competing one.

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

After the share/deploy is up:

```
curl -sI https://<the-public-url>/
```

Expect a 200 (or 3xx that lands on a 200). If you get a redirect to
`http://localhost:3000/...` — you violated rule #2 or #4 above; fix
the code and restart.
