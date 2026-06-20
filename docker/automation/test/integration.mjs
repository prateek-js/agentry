// Integration tests for the persistent backends. For each backend whose
// URL is provided via env, runs the same record→list→get→payload-roundtrip
// suite directly against the driver. Backends with no URL are skipped.
//
//   AUTO_TEST_PG, AUTO_TEST_MYSQL, AUTO_TEST_MONGO, AUTO_TEST_REDIS
//
// See run-integration.sh for the docker harness that supplies them.
import assert from 'node:assert/strict'

const BACKENDS = [
  ['postgres', '../src/store/postgres.js', process.env.AUTO_TEST_PG],
  ['mysql', '../src/store/mysql.js', process.env.AUTO_TEST_MYSQL],
  ['mongo', '../src/store/mongo.js', process.env.AUTO_TEST_MONGO],
  ['redis', '../src/store/redis.js', process.env.AUTO_TEST_REDIS],
]

let failures = 0
let ran = 0

async function withRetry(fn, tries = 30, delayMs = 1000) {
  let last
  for (let i = 0; i < tries; i++) {
    try { return await fn() } catch (e) { last = e; await new Promise((r) => setTimeout(r, delayMs)) }
  }
  throw last
}

for (const [kind, modPath, url] of BACKENDS) {
  if (!url) { console.log(`- ${kind}: SKIP (no URL)`); continue }
  ran++
  console.log(`\n== ${kind} ==`)
  try {
    const mod = await import(new URL(modPath, import.meta.url))
    // retry connect+init: containers (esp. mysql) take time to accept conns
    const store = await withRetry(async () => { const s = await mod.create(url); await s.init(); return s })

    const runId = await store.record({ type: 'run', name: 'job', status: 'ok', output: 'hi', duration_ms: 5 })
    const delId = await store.record({ type: 'delivery', name: 'hook', status: 'ok', payload: { a: 1, b: 'x', nested: { y: true } } })
    await store.record({ type: 'run', name: 'other', status: 'error', error: 'boom' })

    const runs = await store.list({ type: 'run' })
    assert.ok(runs.find((r) => r.id === runId), 'run recorded + listed')
    assert.equal(runs[0].started_at >= runs[runs.length - 1].started_at, true, 'newest-first ordering')

    const byName = await store.list({ type: 'run', name: 'job' })
    assert.ok(byName.length === 1 && byName[0].name === 'job', 'name filter works')

    const dels = await store.list({ type: 'delivery' })
    const del = dels.find((d) => d.id === delId)
    assert.ok(del, 'delivery listed')
    assert.deepEqual(del.payload, { a: 1, b: 'x', nested: { y: true } }, 'delivery payload round-trips')

    const got = await store.get(delId)
    assert.deepEqual(got.payload, { a: 1, b: 'x', nested: { y: true } }, 'get() returns payload for replay')

    console.log(`  ✓ ${kind}: record / list / filter / get / payload round-trip`)

    // Cross-app isolation: two apps (different suffixes) on the SAME url
    // must never see each other's history.
    const a = await withRetry(async () => { const s = await mod.create(url, 'appone'); await s.init(); return s })
    const b = await withRetry(async () => { const s = await mod.create(url, 'apptwo'); await s.init(); return s })
    const aId = await a.record({ type: 'run', name: 'isolated', status: 'ok', output: 'A' })
    const bId = await b.record({ type: 'run', name: 'isolated', status: 'ok', output: 'B' })
    const aRuns = await a.list({ type: 'run' })
    const bRuns = await b.list({ type: 'run' })
    assert.ok(aRuns.find((r) => r.id === aId), 'app A sees its own run')
    assert.ok(!aRuns.find((r) => r.id === bId), 'app A does NOT see app B run')
    assert.ok(bRuns.find((r) => r.id === bId), 'app B sees its own run')
    assert.ok(!bRuns.find((r) => r.id === aId), 'app B does NOT see app A run')
    assert.equal(await a.get(bId), null, 'app A get() cannot fetch app B record')
    console.log(`  ✓ ${kind}: per-app isolation (no cross-app leakage)`)
  } catch (e) {
    failures++
    console.error(`  ✗ ${kind}: ${e.stack || e.message}`)
  }
}

console.log(`\n${ran} backend(s) tested, ${failures} failed`)
process.exit(failures ? 1 : 0)
