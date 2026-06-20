// Your webhooks. withWebhook(name, handler, { verify }) is served at
// POST /api/hooks/<name> and every delivery (payload + result) shows in
// the control panel, with a "Replay" button.
//
// verify: "stripe" | "github" | "slack" checks the provider signature
// using STRIPE_WEBHOOK_SECRET / GITHUB_WEBHOOK_SECRET / SLACK_SIGNING_SECRET.
import { withWebhook } from '@agentry/automation'

export const sample = withWebhook('sample', async ({ body, log }: any) => {
  log('received webhook', body)
  // TODO: handle the event. Return a string (response body) or throw.
  return 'ok'
}, { verify: 'none' })
