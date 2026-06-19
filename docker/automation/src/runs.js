// run tracking — the heart of observability. Wraps any job/handler
// invocation, captures logs + duration + status, and writes one record
// to the bound store on completion. In-flight runs are held in process
// memory so the dashboard can show "running now" without per-backend
// update logic (the persistent stores are write-once on completion).
import { randomUUID } from 'node:crypto'
import { getStore } from './store/index.js'

const inflight = new Map() // id -> { name, type, started_at }

// track runs fn(ctx, payload), records the outcome, returns a summary.
// ctx.log(...) accumulates lines that become the record's output (and
// also stream to stdout, so they show in deployment logs too).
export async function track(name, type, fn, { payload } = {}) {
  const startedAt = new Date()
  const liveId = randomUUID()
  inflight.set(liveId, { name, type, started_at: startedAt.toISOString() })

  const logs = []
  const ctx = {
    log: (...a) => { const s = a.map(stringify).join(' '); logs.push(s); console.log(`[${name}]`, ...a) },
    payload,
  }

  let status = 'ok'
  let error = null
  let returned
  try {
    returned = await fn(ctx, payload)
  } catch (e) {
    status = 'error'
    error = e?.stack || String(e)
    console.error(`[${name}] failed:`, error)
  } finally {
    inflight.delete(liveId)
  }

  const finishedAt = new Date()
  const output = typeof returned === 'string' && returned ? returned : logs.join('\n')
  try {
    const store = await getStore()
    await store.record({
      name, type, status,
      started_at: startedAt.toISOString(),
      finished_at: finishedAt.toISOString(),
      duration_ms: finishedAt - startedAt,
      output, payload: payload ?? null, error,
    })
  } catch (e) {
    console.error(`[agentry/automation] failed to record run for ${name}: ${e?.message}`)
  }
  return { status, output, error }
}

// inFlight lists currently-running invocations (for the live dashboard).
export function inFlight() {
  return [...inflight.values()]
}

function stringify(v) {
  if (typeof v === 'string') return v
  try { return JSON.stringify(v) } catch { return String(v) }
}
