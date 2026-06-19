import { replayDelivery } from '@agentry/automation'
import '@/automations/hooks'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

export async function POST(req: Request) {
  const { id } = await req.json()
  try {
    const run = await replayDelivery(id)
    return Response.json({ ok: true, run })
  } catch (e) {
    return Response.json({ ok: false, error: String((e as Error).message ?? e) }, { status: 400 })
  }
}
