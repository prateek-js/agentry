import { dashboardData } from '@agentry/automation'
import { RunNow, Replay } from './ui'
import '@/automations/jobs'
import '@/automations/hooks'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

function when(iso: string) {
  const d = new Date(iso)
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

export default async function ControlPanel() {
  const d = await dashboardData({ limit: 50 })
  return (
    <main className="wrap">
      <div className="h1"><span className="dot" /> Automation control panel</div>
      <div className="sub">storage: {d.storage.backend}{d.storage.ephemeral ? ' (ephemeral)' : ''}</div>

      {d.storage.ephemeral && (
        <div className="banner">
          Run history is in-memory and resets on every redeploy. Bind a database
          (<span className="mono">agentry service bind postgres</span>) to keep it.
        </div>
      )}

      <section className="section">
        <h2>Schedules</h2>
        <div className="card">
          {d.schedules.length === 0 && <div className="empty">No schedules defined.</div>}
          {d.schedules.map((s: any) => (
            <div className="row" key={s.name}>
              <span className="name">{s.name}</span>
              <span className="mono">{s.cron}</span>
              <span className="spacer muted">{s.next ? `next ${when(s.next)}` : 'not scheduled'}</span>
              <RunNow name={s.name} />
            </div>
          ))}
        </div>
      </section>

      {d.running.length > 0 && (
        <section className="section">
          <h2>Running now</h2>
          <div className="card">
            {d.running.map((r: any, i: number) => (
              <div className="row" key={i}>
                <span className="name">{r.name}</span>
                <span className="pill ok">running</span>
                <span className="spacer muted">since {when(r.started_at)}</span>
              </div>
            ))}
          </div>
        </section>
      )}

      <section className="section">
        <h2>Runs</h2>
        <div className="card">
          {d.runs.length === 0 && <div className="empty">No runs yet. Use “Run now” above.</div>}
          {d.runs.map((r: any) => (
            <details key={r.id}>
              <summary className="row">
                <span className={`pill ${r.status === 'ok' ? 'ok' : 'err'}`}>{r.status}</span>
                <span className="name">{r.name}</span>
                <span className="spacer muted">{when(r.started_at)} · {r.duration_ms}ms</span>
              </summary>
              {(r.output || r.error) && <pre className="payload">{r.error || r.output}</pre>}
            </details>
          ))}
        </div>
      </section>

      <section className="section">
        <h2>Webhook deliveries</h2>
        <div className="card">
          {d.webhooks.length === 0 && d.deliveries.length === 0 && (
            <div className="empty">No webhooks defined.</div>
          )}
          {d.deliveries.map((r: any) => (
            <details key={r.id}>
              <summary className="row">
                <span className={`pill ${r.status === 'ok' ? 'ok' : 'err'}`}>{r.status}</span>
                <span className="name">{r.name}</span>
                <span className="spacer muted">{when(r.started_at)}</span>
                <Replay id={r.id} />
              </summary>
              <pre className="payload">{JSON.stringify(r.payload, null, 2)}</pre>
            </details>
          ))}
        </div>
      </section>
    </main>
  )
}
