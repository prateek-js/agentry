# Services & environment model

How an app in a sandbox gets the things it needs to run — a database URL, an
API key, a Stripe secret — and where those values live.

## The one thing to understand

**Credentials and environment variables are wired straight into the runtime
on your own infrastructure. agentry's platform never sees them.**

A *service binding* (`postgres`, `openai`, `stripe`, …) and a *secret*
(`OPENAI_API_KEY`, …) are just named values your app reads from its
environment. When you set one, the value is written **directly into the
sandbox** by the component running on your machine — it never travels to,
passes through, or is stored by any agentry-operated service. There is no
central vault, no "connect your database to agentry," no copy of your
secrets on our side. The platform has no concept of your bindings; it only
knows a sandbox exists.

```
you bind a value  ──►  it's written into your sandbox  ──►  your app reads it as env
   (on your box)          (on your box / your server)        ($DATABASE_URL, …)
```

## What that means in practice

- **Values live in exactly two places you control:** on the machine you ran
  the bind from, and inside the sandbox itself. Nowhere else.
- **The platform stores names, never values.** At most, a sandbox or
  deployment records *that* it uses a `DATABASE_URL` — never what it is. The
  open-source engine doesn't involve any control plane at all.
- **Bindings appear as ordinary environment variables.** Your app doesn't
  need an agentry SDK or to know any agentry path — it reads `process.env` /
  `os.environ` like it would anywhere. Standard file conventions
  (`AWS_SHARED_CREDENTIALS_FILE`, etc.) work too.
- **New sandboxes come pre-wired.** Values you've set as defaults for a
  server are re-applied to each new sandbox on that server automatically — by
  your own tooling, over your own connection.

## Bind a service

```sh
agentry service ls                  # what the catalog supports
agentry service bind postgres       # interactive — paste your connection URL
agentry env set OPENAI_API_KEY      # a one-off secret (hidden prompt)
```

The value goes into the sandbox; your app sees `DATABASE_URL` /
`OPENAI_API_KEY` on its next start. That's the whole model.

## When you deploy

The same principle holds. At deploy time the values are resolved and
injected straight into the deployed app's container as real environment
variables; only the *names* are recorded so the dashboard can show "this app
ships a `DATABASE_URL`." The values flow through in memory and are never
persisted by the platform.

---

This is a deliberate design choice, and it's the backbone of agentry's
[security model](SECURITY-MODEL.md): your secrets are your sandbox's, not
ours.
