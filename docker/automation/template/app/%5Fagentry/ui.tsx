'use client'
import { useState, useTransition } from 'react'
import { useRouter } from 'next/navigation'

function useAction(url: string, body: object) {
  const router = useRouter()
  const [pending, start] = useTransition()
  const [busy, setBusy] = useState(false)
  const fire = () => {
    setBusy(true)
    fetch(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })
      .finally(() => { setBusy(false); start(() => router.refresh()) })
  }
  return { fire, busy: busy || pending }
}

export function RunNow({ name }: { name: string }) {
  const { fire, busy } = useAction('/_agentry/run', { name })
  return <button className="btn" disabled={busy} onClick={fire}>{busy ? 'Running…' : 'Run now'}</button>
}

export function Replay({ id }: { id: string }) {
  const { fire, busy } = useAction('/_agentry/replay', { id })
  return <button className="btn" disabled={busy} onClick={(e) => { e.preventDefault(); fire() }}>{busy ? 'Replaying…' : 'Replay'}</button>
}
