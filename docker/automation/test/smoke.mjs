// Smoke test: selection logic + memory store + runs + scheduler +
// webhook signature verify, end to end on the in-memory backend.
// The 4 DB backends get container-based integration tests separately.
process.env.AGENTRY_AUTOMATION_STORE = 'memory'

import assert from 'node:assert/strict'
import crypto from 'node:crypto'
import { pickBackend, resetStoreForTests } from '../src/store/index.js'
import * as memory from '../src/store/memory.js'
import { track } from '../src/runs.js'
import { defineSchedule, listSchedules, runScheduleNow } from '../src/scheduler.js'
import { verify } from '../src/verify.js'
import { dashboardData } from '../src/index.js'

let failures = 0
const ok = (name) => console.log(`  ✓ ${name}`)
const check = (name, fn) => { try { fn(); ok(name) } catch (e) { failures++; console.error(`  ✗ ${name}\n    ${e.message}`) } }

// --- selection priority + scheme parsing ---
check('selection: POSTGRES_URL → postgres', () =>
  assert.equal(pickBackend({ POSTGRES_URL: 'x', REDIS_URL: 'y' }).kind, 'postgres'))
check('selection: MYSQL_URL → mysql', () =>
  assert.equal(pickBackend({ MYSQL_URL: 'x' }).kind, 'mysql'))
check('selection: MONGODB_URL → mongo', () =>
  assert.equal(pickBackend({ MONGODB_URL: 'x' }).kind, 'mongo'))
check('selection: REDIS_URL → redis', () =>
  assert.equal(pickBackend({ REDIS_URL: 'x' }).kind, 'redis'))
check('selection: override wins', () =>
  assert.equal(pickBackend({ AGENTRY_AUTOMATION_STORE: 'redis', POSTGRES_URL: 'x' }).kind, 'redis'))
check('selection: DATABASE_URL by scheme (mysql)', () =>
  assert.equal(pickBackend({ DATABASE_URL: 'mysql://u@h/d' }).kind, 'mysql'))
check('selection: nothing → memory', () =>
  assert.equal(pickBackend({}).kind, 'memory'))

// --- memory backend record/list/get ---
await (async () => {
  const s = await memory.create(null); await s.init()
  const id = await s.record({ type: 'run', name: 'job1', status: 'ok', output: 'hello' })
  await s.record({ type: 'delivery', name: 'hook1', status: 'ok', payload: { a: 1 } })
  check('memory: list runs', async () => assert.equal((await s.list({ type: 'run' })).length, 1))
  check('memory: get by id returns payload-bearing record', async () => {
    const got = await s.get(id); assert.equal(got.output, 'hello')
  })
})()

// --- runs.track records to the (memory) store ---
resetStoreForTests()
await (async () => {
  const r = await track('nightly', 'run', (ctx) => { ctx.log('did work'); return 'done' })
  check('track: returns ok', () => assert.equal(r.status, 'ok'))
  const d = await dashboardData()
  check('track: shows in dashboard runs', () => assert.ok(d.runs.find((x) => x.name === 'nightly')))
  check('dashboard: reports memory + ephemeral', () =>
    assert.equal(d.storage.ephemeral, true) || assert.equal(d.storage.backend, 'memory'))
  const err = await track('boom', 'run', () => { throw new Error('nope') })
  check('track: captures errors', () => assert.equal(err.status, 'error'))
})()

// --- scheduler ---
defineSchedule('hourly', '0 * * * *', () => 'tick')
check('scheduler: lists with next fire', () => {
  const list = listSchedules()
  const j = list.find((x) => x.name === 'hourly')
  assert.ok(j && j.cron === '0 * * * *' && j.next)
})
await (async () => {
  const r = await runScheduleNow('hourly')
  check('scheduler: run now executes', () => assert.equal(r.status, 'ok'))
})()

// --- webhook signature verify (github known scheme) ---
check('verify: github valid', () => {
  process.env.GITHUB_WEBHOOK_SECRET = 'topsecret'
  const raw = '{"hello":"world"}'
  const sig = 'sha256=' + crypto.createHmac('sha256', 'topsecret').update(raw).digest('hex')
  assert.equal(verify('github', { rawBody: raw, getHeader: (h) => h === 'x-hub-signature-256' ? sig : null }), true)
})
check('verify: github tampered → false', () => {
  assert.equal(verify('github', { rawBody: 'tampered', getHeader: () => 'sha256=deadbeef' }), false)
})
check('verify: none always passes', () => assert.equal(verify('none', {}), true))
check('verify: missing secret fails closed', () => {
  delete process.env.STRIPE_WEBHOOK_SECRET
  assert.equal(verify('stripe', { rawBody: 'x', getHeader: () => 'y' }), false)
})

console.log(failures ? `\n${failures} FAILED` : '\nall passed')
process.exit(failures ? 1 : 0)
