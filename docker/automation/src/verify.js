// Webhook signature verification presets. Each reads the provider's
// signing secret from a conventional env var and checks the raw body
// against the provider's header scheme with a constant-time compare.
import crypto from 'node:crypto'

const SECRET_ENV = {
  stripe: 'STRIPE_WEBHOOK_SECRET',
  github: 'GITHUB_WEBHOOK_SECRET',
  slack: 'SLACK_SIGNING_SECRET',
}

export function secretEnvFor(kind) { return SECRET_ENV[kind] }

// verify(kind, { rawBody, getHeader }) → boolean. "none" always passes.
// Missing secret → fail closed (false) so a misconfigured webhook is
// rejected loudly rather than silently unverified.
export function verify(kind, { rawBody, getHeader }) {
  if (!kind || kind === 'none') return true
  const secret = process.env[SECRET_ENV[kind] || '']
  if (!secret) return false
  if (kind === 'stripe') return verifyStripe(rawBody, getHeader, secret)
  if (kind === 'github') return verifyGithub(rawBody, getHeader, secret)
  if (kind === 'slack') return verifySlack(rawBody, getHeader, secret)
  return false
}

function timingEqual(a, b) {
  const ba = Buffer.from(a)
  const bb = Buffer.from(b)
  return ba.length === bb.length && crypto.timingSafeEqual(ba, bb)
}

function verifyStripe(raw, getHeader, secret) {
  const sig = getHeader('stripe-signature') || ''
  const parts = Object.fromEntries(sig.split(',').map((kv) => kv.split('=')))
  if (!parts.t || !parts.v1) return false
  const expected = crypto.createHmac('sha256', secret).update(`${parts.t}.${raw}`).digest('hex')
  return timingEqual(expected, parts.v1)
}

function verifyGithub(raw, getHeader, secret) {
  const sig = getHeader('x-hub-signature-256') || ''
  const expected = 'sha256=' + crypto.createHmac('sha256', secret).update(raw).digest('hex')
  return timingEqual(expected, sig)
}

function verifySlack(raw, getHeader, secret) {
  const ts = getHeader('x-slack-request-timestamp') || ''
  const sig = getHeader('x-slack-signature') || ''
  if (!ts) return false
  // Reject requests older than 5 minutes (replay protection).
  if (Math.abs(Date.now() / 1000 - Number(ts)) > 300) return false
  const expected = 'v0=' + crypto.createHmac('sha256', secret).update(`v0:${ts}:${raw}`).digest('hex')
  return timingEqual(expected, sig)
}
