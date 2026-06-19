// Storage adapter for automation run + webhook-delivery records.
//
// One tiny interface, five backends. The bound DB is the source of
// truth (your data, your hardware); with nothing bound we fall back to
// in-memory and the dashboard says so. Records are append-only and
// written on completion — in-flight "running" rows come from process
// memory (see ../runs.js), so every backend stays write-once and simple.
//
// Interface every backend implements:
//   init()                          → ensure table/collection/stream
//   record(entry)                   → persist one record, returns its id
//   list({type,name,limit,before})  → newest-first history
//   get(id)                         → one record (for webhook replay)
//
// Backend selection (first match wins), overridable with
// AGENTRY_AUTOMATION_STORE = postgres | mysql | mongo | redis | memory:
//   POSTGRES_URL → pg ·  MYSQL_URL → mysql ·  MONGODB_URL → mongo ·
//   REDIS_URL → redis ·  DATABASE_URL (by scheme) ·  else memory.
//
// Drivers are dynamic-imported so only the selected one ever loads.

import { randomUUID } from 'node:crypto'

// normalizeEntry fills defaults so every backend stores the same shape.
export function normalizeEntry(e) {
  const now = new Date().toISOString()
  return {
    id: e.id || randomUUID(),
    type: e.type === 'delivery' ? 'delivery' : 'run',
    name: String(e.name || 'default'),
    status: e.status === 'error' ? 'error' : 'ok',
    started_at: e.started_at || e.startedAt || now,
    finished_at: e.finished_at || e.finishedAt || now,
    duration_ms: Number.isFinite(e.duration_ms ?? e.durationMs) ? (e.duration_ms ?? e.durationMs) : 0,
    output: e.output == null ? '' : String(e.output),
    payload: e.payload === undefined ? null : e.payload,
    error: e.error == null ? null : String(e.error),
  }
}

// pickBackend resolves which backend to use from the environment.
// Returns { kind, url } — url is null for memory.
export function pickBackend(env = process.env) {
  const override = (env.AGENTRY_AUTOMATION_STORE || '').toLowerCase().trim()
  if (override === 'memory') return { kind: 'memory', url: null }
  if (override === 'postgres' || override === 'pg') return { kind: 'postgres', url: env.POSTGRES_URL || env.DATABASE_URL }
  if (override === 'mysql') return { kind: 'mysql', url: env.MYSQL_URL || env.DATABASE_URL }
  if (override === 'mongo' || override === 'mongodb') return { kind: 'mongo', url: env.MONGODB_URL || env.DATABASE_URL }
  if (override === 'redis') return { kind: 'redis', url: env.REDIS_URL || env.DATABASE_URL }

  if (env.POSTGRES_URL) return { kind: 'postgres', url: env.POSTGRES_URL }
  if (env.MYSQL_URL) return { kind: 'mysql', url: env.MYSQL_URL }
  if (env.MONGODB_URL) return { kind: 'mongo', url: env.MONGODB_URL }
  if (env.REDIS_URL) return { kind: 'redis', url: env.REDIS_URL }

  // Single ambiguous DATABASE_URL — decide by scheme.
  const db = env.DATABASE_URL || ''
  if (/^postgres(ql)?:\/\//i.test(db)) return { kind: 'postgres', url: db }
  if (/^mysql:\/\//i.test(db)) return { kind: 'mysql', url: db }
  if (/^mongodb(\+srv)?:\/\//i.test(db)) return { kind: 'mongo', url: db }
  if (/^rediss?:\/\//i.test(db)) return { kind: 'redis', url: db }

  return { kind: 'memory', url: null }
}

const loaders = {
  postgres: () => import('./postgres.js'),
  mysql: () => import('./mysql.js'),
  mongo: () => import('./mongo.js'),
  redis: () => import('./redis.js'),
  memory: () => import('./memory.js'),
}

let cached = null

// getStore returns the process-wide store singleton, initialized once.
// If the chosen backend fails to connect/init, we log loudly and fall
// back to memory so an automation never crash-loops over storage.
export async function getStore(env = process.env) {
  if (cached) return cached
  const choice = pickBackend(env)
  try {
    const mod = await loaders[choice.kind]()
    const store = await mod.create(choice.url)
    await store.init()
    store.kind = choice.kind
    store.ephemeral = choice.kind === 'memory'
    cached = store
  } catch (err) {
    console.error(`[agentry/automation] store backend ${choice.kind} failed (${err?.message}); falling back to in-memory`)
    const mem = await loaders.memory()
    cached = await mem.create(null)
    await cached.init()
    cached.kind = 'memory'
    cached.ephemeral = true
  }
  return cached
}

// resetStoreForTests drops the singleton (test-only).
export function resetStoreForTests() { cached = null }
