// Runs once when the Next server boots (Node runtime only). This is
// where schedules get armed and webhooks get registered so the control
// panel can list them — Next loads route modules lazily, so importing
// the automation definitions here is what makes them live at startup.
export async function register() {
  if (process.env.NEXT_RUNTIME !== 'nodejs') return
  await import('./automations/jobs')
  await import('./automations/hooks')
}
