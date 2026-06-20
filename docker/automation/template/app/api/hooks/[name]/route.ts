import { dispatch } from '@agentry/automation'
import '@/automations/hooks' // ensure handlers are registered

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

// One route serves every webhook: POST /api/hooks/<name> → the handler
// registered under <name> in automations/hooks.ts.
export async function POST(req: Request, { params }: { params: { name: string } }) {
  return dispatch(params.name, req)
}
