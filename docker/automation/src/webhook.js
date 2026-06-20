// Webhooks — wrap a handler into a Next.js route-handler-compatible POST
// function. Verifies the signature (preset), records every delivery as a
// run with the payload attached (so the control panel can show + replay
// it), and returns the handler's response.
import { track } from './runs.js'
import { verify } from './verify.js'
import { getStore } from './store/index.js'

const webhooks = new Map() // name -> { handler, verify }

// withWebhook(name, handler, { verify: "stripe|github|slack" }).
// handler({ body, raw, headers, log }) → string | Response | void.
export function withWebhook(name, handler, opts = {}) {
  const kind = opts.verify || 'none'
  const post = async function POST(request) {
    const raw = await request.text()
    if (kind !== 'none') {
      const ok = verify(kind, { rawBody: raw, getHeader: (h) => request.headers.get(h) })
      if (!ok) return new Response('invalid signature', { status: 401 })
    }
    let body
    try { body = raw ? JSON.parse(raw) : null } catch { body = raw }

    const res = await track(name, 'delivery', async (ctx) => {
      const out = await handler({ body, raw, headers: Object.fromEntries(request.headers), log: ctx.log })
      if (out instanceof Response) return out
      return typeof out === 'string' ? out : ''
    }, { payload: body })

    if (res.status === 'error') return new Response('handler error', { status: 500 })
    return new Response(res.output || 'ok', { status: 200 })
  }
  webhooks.set(name, { handler, verify: kind, post })
  return post
}

export function listWebhooks() {
  return [...webhooks.entries()].map(([name, w]) => ({ name, verify: w.verify }))
}

// dispatch routes an incoming request to the webhook registered under
// `name` — lets one dynamic Next route (/api/hooks/[name]) serve every
// webhook the automation defines, so the agent never wires routes by hand.
export async function dispatch(name, request) {
  const w = webhooks.get(name)
  if (!w) return new Response(`no webhook named ${name}`, { status: 404 })
  return w.post(request)
}

// replayDelivery re-runs a stored delivery's handler with its payload —
// the dashboard "Replay" button. Records a fresh delivery record.
export async function replayDelivery(id) {
  const store = await getStore()
  const e = await store.get(id)
  if (!e || e.type !== 'delivery') throw new Error(`no delivery ${id}`)
  const wh = webhooks.get(e.name)
  if (!wh) throw new Error(`no webhook named ${e.name}`)
  return track(e.name, 'delivery', async (ctx) => {
    const out = await wh.handler({ body: e.payload, raw: JSON.stringify(e.payload ?? null), headers: {}, log: ctx.log })
    return typeof out === 'string' ? out : ''
  }, { payload: e.payload })
}
