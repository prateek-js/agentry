// Scheduler — register cron jobs that run inside the deployment process.
// Call defineSchedule() from Next's instrumentation.ts (runs once on
// server boot). Single-instance only: agentry deployments are one
// long-running container, so in-process cron won't double-fire.
import { Cron } from 'croner'
import { track } from './runs.js'

const schedules = new Map() // name -> { expr, job, fn }

// defineSchedule(name, cronExpr, fn). Idempotent on name so a hot-reload
// (Next dev) doesn't stack duplicate timers.
export function defineSchedule(name, expr, fn) {
  if (schedules.has(name)) {
    schedules.get(name).job.stop()
    schedules.delete(name)
  }
  // AGENTRY_RUN_NOW=1 fires every schedule once at boot — the in-sandbox
  // test loop ("does my job work?") without waiting for the cron.
  if (process.env.AGENTRY_RUN_NOW === '1') {
    setTimeout(() => track(name, 'run', fn), 100)
  }
  const job = new Cron(expr, { name }, () => track(name, 'run', fn))
  schedules.set(name, { expr, job, fn })
  return job
}

// listSchedules — for the control panel: cron expr + next fire time.
export function listSchedules() {
  return [...schedules.entries()].map(([name, s]) => ({
    name,
    cron: s.expr,
    next: s.job.nextRun()?.toISOString() || null,
  }))
}

// runScheduleNow — the dashboard "Run now" button + the test loop.
export async function runScheduleNow(name) {
  const s = schedules.get(name)
  if (!s) throw new Error(`no schedule named ${name}`)
  return track(name, 'run', s.fn)
}
