import { dashboardData } from '@agentry/automation'
import '@/automations/jobs'
import '@/automations/hooks'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

// The control panel polls this for live state (runs, deliveries, schedules,
// webhooks, what's running now). Importing the automation modules here keeps
// the listing correct even if this route is hit before instrumentation runs.
export async function GET() {
  const d = await dashboardData({ limit: 200 })
  return Response.json(d, { headers: { 'cache-control': 'no-store' } })
}
