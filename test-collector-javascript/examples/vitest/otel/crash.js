import { test } from 'vitest'
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
// A Vitest worker sends results at most once per 100 ms, and its throttle can
// drop the trailing send. Finishing after more than two windows sends this
// result immediately rather than relying on that trailing send.
test('passes before worker crashes', () => sleep(250))
test('worker crashes', async () => {
  // Let the result reach the main process before killing the worker. Vitest's
  // process.exit wrapper throws instead of exercising genuine worker death.
  await sleep(200)
  if (process.env.OTEL_TEST_POOL === 'threads') process.reallyExit(1)
  else process.kill(process.pid, 'SIGKILL')
})
test('never reached', () => {})
