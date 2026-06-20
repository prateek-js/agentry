// Your scheduled jobs. Each defineSchedule(name, cron, fn) runs in-process
// on the cron — and shows up in the control panel at /_agentry with run
// history, next-fire time, and a "Run now" button.
//
// ctx.log(...) lines become the run's output. Throw to mark a run failed.
import { defineSchedule } from '@agentry/automation'

defineSchedule('daily-digest', '0 9 * * *', async (ctx: any) => {
  ctx.log('running the daily digest…')
  // TODO: do the work — read a bound DB, call an API, post to Slack, etc.
  return 'digest complete'
})
