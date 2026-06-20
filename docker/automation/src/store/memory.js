// In-memory store: the fallback when no DB is bound. Ephemeral — every
// container restart/redeploy starts empty, and the dashboard surfaces a
// "bind a database to persist" banner. Capped (unlike the persistent
// backends) so an unbound automation can't slowly OOM its container.
import { normalizeEntry } from './index.js'

const CAP = 2000

export async function create() {
  const rows = [] // newest pushed to the end
  return {
    async init() {},
    async record(entry) {
      const e = normalizeEntry(entry)
      rows.push(e)
      if (rows.length > CAP) rows.splice(0, rows.length - CAP)
      return e.id
    },
    async list({ type, name, limit = 100, before } = {}) {
      let out = rows
      if (type) out = out.filter((r) => r.type === type)
      if (name) out = out.filter((r) => r.name === name)
      if (before) out = out.filter((r) => r.started_at < before)
      return out.slice(-limit).reverse()
    },
    async get(id) {
      return rows.find((r) => r.id === id) || null
    },
  }
}
