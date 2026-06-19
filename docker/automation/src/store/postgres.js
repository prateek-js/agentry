// Postgres backend. Append-only; no retention (it's the customer's DB —
// their storage, their call). Table is namespaced so it never collides
// with the app's own schema. payload is JSONB for queryable deliveries.
import { normalizeEntry } from './index.js'

const TABLE = '_agentry_runs'

export async function create(url) {
  const { default: pg } = await import('pg')
  const pool = new pg.Pool({ connectionString: url, max: 4 })
  return {
    async init() {
      await pool.query(`
        CREATE TABLE IF NOT EXISTS ${TABLE} (
          id           text PRIMARY KEY,
          type         text NOT NULL,
          name         text NOT NULL,
          status       text NOT NULL,
          started_at   timestamptz NOT NULL,
          finished_at  timestamptz NOT NULL,
          duration_ms  integer NOT NULL DEFAULT 0,
          output       text NOT NULL DEFAULT '',
          payload      jsonb,
          error        text
        );
        CREATE INDEX IF NOT EXISTS ${TABLE}_started_idx ON ${TABLE} (type, started_at DESC);
      `)
    },
    async record(entry) {
      const e = normalizeEntry(entry)
      await pool.query(
        `INSERT INTO ${TABLE} (id,type,name,status,started_at,finished_at,duration_ms,output,payload,error)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
         ON CONFLICT (id) DO NOTHING`,
        [e.id, e.type, e.name, e.status, e.started_at, e.finished_at, e.duration_ms, e.output,
         e.payload === null ? null : JSON.stringify(e.payload), e.error],
      )
      return e.id
    },
    async list({ type, name, limit = 100, before } = {}) {
      const where = []
      const args = []
      if (type) { args.push(type); where.push(`type = $${args.length}`) }
      if (name) { args.push(name); where.push(`name = $${args.length}`) }
      if (before) { args.push(before); where.push(`started_at < $${args.length}`) }
      args.push(Math.min(limit, 1000))
      const sql = `SELECT * FROM ${TABLE} ${where.length ? 'WHERE ' + where.join(' AND ') : ''}
                   ORDER BY started_at DESC LIMIT $${args.length}`
      const { rows } = await pool.query(sql, args)
      return rows.map(rowToEntry)
    },
    async get(id) {
      const { rows } = await pool.query(`SELECT * FROM ${TABLE} WHERE id = $1`, [id])
      return rows[0] ? rowToEntry(rows[0]) : null
    },
  }
}

function rowToEntry(r) {
  return {
    id: r.id, type: r.type, name: r.name, status: r.status,
    started_at: new Date(r.started_at).toISOString(),
    finished_at: new Date(r.finished_at).toISOString(),
    duration_ms: r.duration_ms, output: r.output,
    payload: r.payload ?? null, error: r.error ?? null,
  }
}
