'use client'
import { useCallback, useEffect, useState } from 'react'

type View =
  | { kind: 'overview' }
  | { kind: 'schedule'; name: string }
  | { kind: 'webhook'; name: string }

type Filter = 'all' | 'ok' | 'error'

function when(iso: string) {
  if (!iso) return ''
  return new Date(iso).toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}
function dur(ms: number) {
  if (ms == null) return ''
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
}

function Pill({ status }: { status: string }) {
  const ok = status === 'ok'
  return <span className={`pill ${ok ? 'ok' : 'err'}`}>{ok ? 'ok' : 'error'}</span>
}

// Hoisted to module scope (not redefined per render) so the 4s poll doesn't
// remount open rows and reset log scroll/selection mid-read.
function ActivityRow({ r, type, open, onToggle, busy, onReplay }: {
  r: any; type: 'run' | 'delivery'; open: boolean; onToggle: () => void
  busy: boolean; onReplay: () => void
}) {
  return (
    <div className={`logrow ${open ? 'open' : ''}`}>
      <button className="logrow-head" onClick={onToggle}>
        <span className="caret">{open ? '▾' : '▸'}</span>
        <Pill status={r.status} />
        <span className="name">{r.name}</span>
        <span className="tag">{type === 'run' ? 'run' : 'webhook'}</span>
        <span className="spacer" />
        <span className="muted">{when(r.started_at)}{type === 'run' && r.duration_ms != null ? ` · ${dur(r.duration_ms)}` : ''}</span>
      </button>
      {open && (
        <div className="logrow-body">
          {type === 'delivery' && (
            <>
              <div className="lbl">Payload</div>
              <pre className="log">{JSON.stringify(r.payload, null, 2)}</pre>
              <div className="actions">
                <button className="btn" disabled={busy} onClick={onReplay}>{busy ? 'Replaying…' : 'Replay'}</button>
              </div>
            </>
          )}
          <div className="lbl">{r.status === 'error' ? 'Error' : 'Output'}</div>
          <pre className={`log ${r.status === 'error' ? 'log-err' : ''}`}>{r.error || r.output || '(no output)'}</pre>
        </div>
      )}
    </div>
  )
}

function Filters({ filter, setFilter }: { filter: Filter; setFilter: (f: Filter) => void }) {
  return (
    <div className="filters">
      {(['all', 'ok', 'error'] as const).map((f) => (
        <button key={f} className={`chip ${filter === f ? 'on' : ''}`} onClick={() => setFilter(f)}>{f}</button>
      ))}
    </div>
  )
}

function LogList({ rows, type, empty, filter, openId, onToggle, busyMap, onReplay }: {
  rows: any[]; type: 'run' | 'delivery'; empty: string; filter: Filter
  openId: string | null; onToggle: (id: string) => void
  busyMap: Record<string, boolean>; onReplay: (id: string) => void
}) {
  const shown = filter === 'all' ? rows : rows.filter((r) => (filter === 'ok' ? r.status === 'ok' : r.status === 'error'))
  if (!shown.length) return <div className="empty">{empty}</div>
  return (
    <div className="loglist">
      {shown.map((r) => (
        <ActivityRow key={r.id} r={r} type={type} open={openId === r.id}
          onToggle={() => onToggle(r.id)} busy={!!busyMap[r.id]} onReplay={() => onReplay(r.id)} />
      ))}
    </div>
  )
}

export function Panel() {
  const [d, setD] = useState<any>(null)
  const [view, setView] = useState<View>({ kind: 'overview' })
  const [openId, setOpenId] = useState<string | null>(null)
  const [filter, setFilter] = useState<Filter>('all')
  const [busy, setBusy] = useState<Record<string, boolean>>({})
  const [origin, setOrigin] = useState('')
  const [copied, setCopied] = useState(false)

  const load = useCallback(async () => {
    try {
      const r = await fetch('/_agentry/data', { cache: 'no-store' })
      if (r.ok) setD(await r.json())
    } catch { /* keep last good state */ }
  }, [])

  useEffect(() => {
    setOrigin(window.location.origin)
    load()
    const t = setInterval(load, 4000)
    return () => clearInterval(t)
  }, [load])

  const act = async (url: string, body: any, key: string) => {
    setBusy((b) => ({ ...b, [key]: true }))
    try {
      await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })
    } catch { /* ignore */ }
    await load()
    setBusy((b) => ({ ...b, [key]: false }))
  }
  const toggle = (id: string) => setOpenId((cur) => (cur === id ? null : id))
  const replay = (id: string) => act('/_agentry/replay', { id }, id)

  if (!d) return <div className="loading"><span className="spin" /> Loading…</div>

  const running: any[] = d.running || []
  const runningNames = new Set(running.map((r) => r.name))
  const runsFor = (name: string) => (d.runs || []).filter((r: any) => r.name === name)
  const delsFor = (name: string) => (d.deliveries || []).filter((r: any) => r.name === name)
  const lastStatus = (rows: any[]) => (rows.length ? rows[0].status : '')

  const select = (v: View) => { setView(v); setOpenId(null); setFilter('all') }

  return (
    <div className="panel">
      <aside className="side">
        <div className="brand"><span className={`live ${d.storage.ephemeral ? 'amber' : ''}`} /> Automation</div>

        <button className={`nav ${view.kind === 'overview' ? 'on' : ''}`} onClick={() => select({ kind: 'overview' })}>Overview</button>

        <div className="navgroup">Schedules <span className="count">{d.schedules.length}</span></div>
        {d.schedules.length === 0 && <div className="navempty">none defined</div>}
        {d.schedules.map((s: any) => (
          <button key={s.name} className={`nav sub ${view.kind === 'schedule' && view.name === s.name ? 'on' : ''}`}
            onClick={() => select({ kind: 'schedule', name: s.name })}>
            {runningNames.has(s.name) ? <span className="live" /> : <span className={`statusdot ${lastStatus(runsFor(s.name))}`} />}
            <span className="navname">{s.name}</span>
          </button>
        ))}

        <div className="navgroup">Webhooks <span className="count">{d.webhooks.length}</span></div>
        {d.webhooks.length === 0 && <div className="navempty">none defined</div>}
        {d.webhooks.map((w: any) => (
          <button key={w.name} className={`nav sub ${view.kind === 'webhook' && view.name === w.name ? 'on' : ''}`}
            onClick={() => select({ kind: 'webhook', name: w.name })}>
            <span className={`statusdot ${lastStatus(delsFor(w.name))}`} />
            <span className="navname">{w.name}</span>
          </button>
        ))}
      </aside>

      <main className="main">
        {d.storage.ephemeral && (
          <div className="banner">
            Run history is in-memory and resets on every redeploy.
            Bind a database — <span className="mono">agentry service bind postgres</span> — to keep it.
          </div>
        )}

        {view.kind === 'overview' && (() => {
          const activity = [
            ...(d.runs || []).map((r: any) => ({ ...r, _t: 'run' as const })),
            ...(d.deliveries || []).map((r: any) => ({ ...r, _t: 'delivery' as const })),
          ].sort((a, b) => (a.started_at < b.started_at ? 1 : -1)).slice(0, 40)
          return (
            <>
              <div className="head">
                <h1>Overview</h1>
                <div className="sub">storage: <strong>{d.storage.backend}</strong>{d.storage.ephemeral ? ' · ephemeral' : ' · persistent'}</div>
              </div>
              <div className="stats">
                <div className="stat"><div className="statn">{d.schedules.length}</div><div className="statl">schedules</div></div>
                <div className="stat"><div className="statn">{d.webhooks.length}</div><div className="statl">webhooks</div></div>
                <div className="stat"><div className="statn">{(d.runs || []).length}</div><div className="statl">recent runs</div></div>
                <div className="stat"><div className="statn">{(d.deliveries || []).length}</div><div className="statl">deliveries</div></div>
              </div>
              <div className="head2">Recent activity</div>
              {activity.length === 0
                ? <div className="empty">Nothing yet. Open a schedule and hit “Run now,” or send a webhook.</div>
                : (
                  <div className="loglist">
                    {activity.map((r: any) => (
                      <ActivityRow key={r.id} r={r} type={r._t} open={openId === r.id}
                        onToggle={() => toggle(r.id)} busy={!!busy[r.id]} onReplay={() => replay(r.id)} />
                    ))}
                  </div>
                )}
            </>
          )
        })()}

        {view.kind === 'schedule' && (() => {
          const s = d.schedules.find((x: any) => x.name === view.name)
          const isRunning = runningNames.has(view.name)
          return (
            <>
              <div className="head">
                <h1>{view.name} {isRunning && <span className="pill running">running</span>}</h1>
                <div className="sub">
                  <span className="mono">{s?.cron || '—'}</span>
                  <span className="dotsep">·</span>
                  {s?.next ? `next run ${when(s.next)}` : 'not scheduled'}
                </div>
              </div>
              <div className="toolbar">
                <button className="btn primary" disabled={busy[view.name]} onClick={() => act('/_agentry/run', { name: view.name }, view.name)}>
                  {busy[view.name] ? 'Running…' : 'Run now'}
                </button>
                <Filters filter={filter} setFilter={setFilter} />
              </div>
              <LogList rows={runsFor(view.name)} type="run" empty="No runs yet. Hit “Run now” to test it."
                filter={filter} openId={openId} onToggle={toggle} busyMap={busy} onReplay={replay} />
            </>
          )
        })()}

        {view.kind === 'webhook' && (() => {
          const w = d.webhooks.find((x: any) => x.name === view.name)
          const url = `${origin}/api/hooks/${view.name}`
          return (
            <>
              <div className="head">
                <h1>{view.name}</h1>
                <div className="sub">signature check: <strong>{w?.verify && w.verify !== 'none' ? w.verify : 'none'}</strong></div>
              </div>
              <div className="urlbar">
                <span className="urllbl">Endpoint</span>
                <code className="url">{url}</code>
                <button className="btn" onClick={() => { navigator.clipboard?.writeText(url); setCopied(true); setTimeout(() => setCopied(false), 1200) }}>
                  {copied ? 'Copied' : 'Copy'}
                </button>
              </div>
              <div className="toolbar"><Filters filter={filter} setFilter={setFilter} /></div>
              <LogList rows={delsFor(view.name)} type="delivery" empty="No deliveries yet. Send a request to the endpoint above."
                filter={filter} openId={openId} onToggle={toggle} busyMap={busy} onReplay={replay} />
            </>
          )
        })()}
      </main>
    </div>
  )
}
