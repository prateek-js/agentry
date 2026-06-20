import { Panel } from './ui'
import '@/automations/jobs'
import '@/automations/hooks'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

// The control panel is a live client view; it polls /_agentry/data. This
// server shell just imports the automation modules so they're registered.
export default function ControlPanel() {
  return <Panel />
}
