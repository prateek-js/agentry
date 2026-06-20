import { runScheduleNow } from '@agentry/automation'
import '@/automations/jobs'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

export async function POST(req: Request) {
  const { name } = await req.json()
  try {
    const run = await runScheduleNow(name)
    return Response.json({ ok: true, run })
  } catch (e) {
    return Response.json({ ok: false, error: String((e as Error).message ?? e) }, { status: 400 })
  }
}
