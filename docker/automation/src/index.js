// @agentry/automation — opinionated automation runtime for agentry.
//
// Build automations as a normal Next.js (port-shape) app:
//   - schedules run in-process (register from instrumentation.ts)
//   - webhooks are Next route handlers
//   - the control panel lives at /_agentry, backed by the bound DB
//
// Persistence follows whatever DB you bind (Postgres/MySQL/Mongo/Redis);
// with nothing bound, run history is in-memory + ephemeral and the
// dashboard says so. Bind one to keep history across redeploys.
export { defineSchedule, listSchedules, runScheduleNow } from './scheduler.js'
export { withWebhook, listWebhooks, replayDelivery } from './webhook.js'
export { track, inFlight } from './runs.js'
export { getStore } from './store/index.js'

import { getStore } from './store/index.js'
import { inFlight } from './runs.js'
import { listSchedules } from './scheduler.js'
import { listWebhooks } from './webhook.js'

// dashboardData aggregates everything the control panel renders, so the
// template's /_agentry pages are a thin view over one call.
export async function dashboardData({ limit = 50 } = {}) {
  const store = await getStore()
  const [runs, deliveries] = await Promise.all([
    store.list({ type: 'run', limit }),
    store.list({ type: 'delivery', limit }),
  ])
  return {
    storage: { backend: store.kind, ephemeral: !!store.ephemeral },
    schedules: listSchedules(),
    webhooks: listWebhooks(),
    runs,
    deliveries,
    running: inFlight(),
  }
}
