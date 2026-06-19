// Redis backend using a Stream — the natural fit for an append log:
// ordered, server-assigned-ish ids, range queries for "newest first",
// and id-fetch for replay. Append-only, no retention (customer's store).
// We keep our own uuid in the entry (for get-by-id) and also index it to
// the stream entry id for O(1) lookup via a companion hash.
import { normalizeEntry } from './index.js'

const STREAM = '_agentry:runs'
const INDEX = '_agentry:runs:byid' // uuid -> stream entry id

export async function create(url) {
  const { default: Redis } = await import('ioredis')
  const client = new Redis(url, { lazyConnect: false, maxRetriesPerRequest: 2 })
  return {
    async init() { /* streams are created on first XADD */ },
    async record(entry) {
      const e = normalizeEntry(entry)
      const sid = await client.xadd(STREAM, '*', 'data', JSON.stringify(e))
      await client.hset(INDEX, e.id, sid)
      return e.id
    },
    async list({ type, name, limit = 100, before } = {}) {
      // XREVRANGE gives newest-first; we over-read and filter in-process
      // (the dashboard's filters are coarse and volumes are small).
      const want = Math.min(limit, 1000)
      const raw = await client.xrevrange(STREAM, '+', '-', 'COUNT', want * 4)
      const out = []
      for (const [, fields] of raw) {
        const e = parseFields(fields)
        if (!e) continue
        if (type && e.type !== type) continue
        if (name && e.name !== name) continue
        if (before && !(e.started_at < before)) continue
        out.push(e)
        if (out.length >= want) break
      }
      return out
    },
    async get(id) {
      const sid = await client.hget(INDEX, id)
      if (!sid) return null
      const raw = await client.xrange(STREAM, sid, sid)
      return raw[0] ? parseFields(raw[0][1]) : null
    },
  }
}

// fields is a flat [k,v,k,v] array; we store everything under "data".
function parseFields(fields) {
  for (let i = 0; i < fields.length; i += 2) {
    if (fields[i] === 'data') { try { return JSON.parse(fields[i + 1]) } catch { return null } }
  }
  return null
}
