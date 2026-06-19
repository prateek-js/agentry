import { redirect } from 'next/navigation'

// By default the automation IS its control panel. If you want this
// automation to also serve its own app UI, replace this with your page —
// the control panel always stays at /_agentry.
export default function Home() {
  redirect('/_agentry')
}
