# Skill: Automation (scheduled jobs + webhooks, with a built-in control panel)

Use when the user says something like "build an automation", "run this
on a schedule", "every morning / hourly / cron", "a worker that polls
X", "handle a Stripe/GitHub/Slack webhook", "do something when an event
comes in". Read this whole page before scaffolding.

## The one idea

**An automation is a normal `kind: nextjs` app** (one project, one port —
CONTRACT.md still holds) that uses the baked `@agentry/automation`
package. You do NOT build a bespoke daemon, a cron container, or your own
logs UI. You get three things for free:

1. **Schedules** that run in-process on a cron expression.
2. **Webhooks** served as Next route handlers, with signature verification.
3. **A control panel at `/_agentry`** — run history, next-fire times,
   webhook payloads, "Run now", and "Replay" — fully templated. Don't
   rebuild it; it renders itself from the run store.

The sandbox is a running computer, so the scheduler is just a cron living
in the app process. No external scheduler, no separate service.

## Scaffold

`project_create kind=automation` does the scaffolding — it copies the
baked `@agentry/automation` template (scheduler + webhook router + control
panel) into your project. Then:

```bash
cd /workspace/projects/<name>
npm install            # @agentry/automation + the DB drivers resolve here
```

Then `project_start`. Iterate with edits + `project_logs`; the control
panel at `/_agentry` is your observability (it's the automation's "Open it").

(Do NOT use `kind: python-script` for a scheduled/webhook job — it has no
schedule primitive and no UI. `kind: automation` is the one.)

What's in the template, and the only files you normally touch:

- `automations/jobs.ts`  — your schedules (`defineSchedule`)
- `automations/hooks.ts` — your webhooks (`withWebhook`)
- everything else (instrumentation, control panel, store, next.config) is
  plumbing — leave it. In particular **do not edit `next.config.mjs`**:
  `output: 'standalone'`, the instrumentation hook, and the DB-driver
  externals are load-bearing. Removing any of them silently stops the
  cron from firing or breaks the build.

## Schedules

```ts
import { defineSchedule } from '@agentry/automation'

defineSchedule('daily-digest', '0 9 * * *', async (ctx) => {
  ctx.log('starting…')          // log lines become the run's output
  // do the work
  return 'digest sent'           // a returned string is the run's output
})                               // throw to mark the run failed
```

- Standard 5-field cron (`min hour dom mon dow`). Times are the server's
  clock (UTC unless the box says otherwise).
- Schedules are armed at server boot by `instrumentation.ts`, which
  imports `automations/jobs.ts`. If you add a *new* file under
  `automations/`, import it there too — otherwise it never loads.
- Every fire is recorded; the panel shows status, duration, output, and
  the next fire time, with a **Run now** button (don't wait for the cron
  to test — click it, or `POST /_agentry/run {"name":"daily-digest"}`).

## Webhooks

```ts
import { withWebhook } from '@agentry/automation'

export const onPush = withWebhook('github-push', async ({ body, log }) => {
  log('event', body.action)
  // handle it
  return 'ok'
}, { verify: 'github' })
```

- Served automatically at **`POST /api/hooks/<name>`** by one dynamic
  route — you do NOT add a route file per webhook. The name you pass is
  the URL segment.
- `verify: 'stripe' | 'github' | 'slack' | 'none'` checks the provider
  signature using `STRIPE_WEBHOOK_SECRET` / `GITHUB_WEBHOOK_SECRET` /
  `SLACK_SIGNING_SECRET` from env. With a preset set, missing secret or
  bad signature fails closed (401). Use `'none'` only for trusted callers.
- Every delivery is recorded with its payload; the panel shows it and
  offers **Replay** (re-runs the handler with the stored payload).

## Storage — bind a DB or it's ephemeral

Runs and deliveries persist to whatever database the operator binds; the
DB is the customer's, so there is no retention policy and no SQLite/local
volume. Supported, picked automatically in this order:

`AGENTRY_AUTOMATION_STORE` (override) → `POSTGRES_URL` → `MYSQL_URL` →
`MONGODB_URL` → `REDIS_URL` → `DATABASE_URL` (by scheme) → **in-memory**.

With nothing bound it falls back to in-memory and the panel says
**ephemeral** — history resets on every redeploy. If the user wants
durable history, tell them to bind one (e.g. `agentry service bind
postgres`); the app picks it up on next start, no code change. Free tiers
worth suggesting: Supabase (Postgres), Neon (Postgres), MongoDB Atlas,
Upstash (Redis), PlanetScale (MySQL).

The store is created lazily and only the matching driver loads, so binding
a different DB later just works. History is namespaced per app (its own
table / keys, derived from the app's identity), so two automations that
bind the same database never see each other's runs.

## Guardrails

- One automation = one project (CONTRACT rule 2). Multiple schedules and
  webhooks live together in the same app.
- Don't pass `--port`/`-p` in the start command — `PORT` is injected
  (CONTRACT rule 1). The scaffold already does this right.
- If the schedule never fires, check it's imported from
  `instrumentation.ts` and that `output: 'standalone'` +
  `instrumentationHook` are still in `next.config.mjs` — that's the usual
  cause. A genuinely broken platform path (panel 500s, store won't
  connect to a bound DB) is PLATFORM territory: capture `project_logs`,
  report it, and stop (CONTRACT rule 5).
