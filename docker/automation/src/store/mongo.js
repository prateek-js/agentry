// MongoDB backend. One namespaced collection; append-only, no retention.
// Stores the normalized entry as-is (payload is a native sub-document).
import { normalizeEntry } from './index.js'

const COLL = '_agentry_runs'

export async function create(url) {
  const { MongoClient } = await import('mongodb')
  const client = new MongoClient(url)
  await client.connect()
  // DB name comes from the connection string; fall back to "agentry".
  const db = client.db()
  const col = db.collection(COLL)
  return {
    async init() {
      await col.createIndex({ type: 1, started_at: -1 })
    },
    async record(entry) {
      const e = normalizeEntry(entry)
      await col.updateOne({ id: e.id }, { $setOnInsert: e }, { upsert: true })
      return e.id
    },
    async list({ type, name, limit = 100, before } = {}) {
      const q = {}
      if (type) q.type = type
      if (name) q.name = name
      if (before) q.started_at = { $lt: before }
      const docs = await col.find(q, { projection: { _id: 0 } })
        .sort({ started_at: -1 }).limit(Math.min(limit, 1000)).toArray()
      return docs
    },
    async get(id) {
      return col.findOne({ id }, { projection: { _id: 0 } })
    },
  }
}
