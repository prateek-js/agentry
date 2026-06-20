// MySQL backend. Same shape/semantics as Postgres (append-only, no
// retention, namespaced table); payload stored as JSON.
import { normalizeEntry } from './index.js'

// Table is per-app (suffix from appNamespace) so two apps sharing one
// database never read each other's history. Empty suffix → legacy name.
export async function create(url, suffix = '') {
  const mysql = await import('mysql2/promise')
  const pool = mysql.createPool(url)
  const TABLE = suffix ? `_agentry_runs_${suffix}` : '_agentry_runs'
  return {
    async init() {
      await pool.query(`
        CREATE TABLE IF NOT EXISTS ${TABLE} (
          id           VARCHAR(64) PRIMARY KEY,
          type         VARCHAR(16) NOT NULL,
          name         VARCHAR(255) NOT NULL,
          status       VARCHAR(16) NOT NULL,
          started_at   DATETIME(3) NOT NULL,
          finished_at  DATETIME(3) NOT NULL,
          duration_ms  INT NOT NULL DEFAULT 0,
          output       MEDIUMTEXT,
          payload      JSON,
          error        TEXT,
          INDEX ${TABLE}_started_idx (type, started_at)
        )`)
    },
    async record(entry) {
      const e = normalizeEntry(entry)
      await pool.query(
        `INSERT IGNORE INTO ${TABLE}
           (id,type,name,status,started_at,finished_at,duration_ms,output,payload,error)
         VALUES (?,?,?,?,?,?,?,?,?,?)`,
        [e.id, e.type, e.name, e.status, isoToMy(e.started_at), isoToMy(e.finished_at),
         e.duration_ms, e.output, e.payload === null ? null : JSON.stringify(e.payload), e.error],
      )
      return e.id
    },
    async list({ type, name, limit = 100, before } = {}) {
      const where = []
      const args = []
      if (type) { where.push('type = ?'); args.push(type) }
      if (name) { where.push('name = ?'); args.push(name) }
      if (before) { where.push('started_at < ?'); args.push(isoToMy(before)) }
      const sql = `SELECT * FROM ${TABLE} ${where.length ? 'WHERE ' + where.join(' AND ') : ''}
                   ORDER BY started_at DESC LIMIT ?`
      args.push(Math.min(limit, 1000))
      const [rows] = await pool.query(sql, args)
      return rows.map(rowToEntry)
    },
    async get(id) {
      const [rows] = await pool.query(`SELECT * FROM ${TABLE} WHERE id = ?`, [id])
      return rows[0] ? rowToEntry(rows[0]) : null
    },
  }
}

const isoToMy = (iso) => new Date(iso).toISOString().slice(0, 23).replace('T', ' ')

function rowToEntry(r) {
  const payload = typeof r.payload === 'string' ? safeJSON(r.payload) : (r.payload ?? null)
  return {
    id: r.id, type: r.type, name: r.name, status: r.status,
    started_at: new Date(r.started_at).toISOString(),
    finished_at: new Date(r.finished_at).toISOString(),
    duration_ms: r.duration_ms, output: r.output ?? '', payload, error: r.error ?? null,
  }
}
const safeJSON = (s) => { try { return JSON.parse(s) } catch { return s } }
