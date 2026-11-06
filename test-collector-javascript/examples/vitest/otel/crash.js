import { test } from 'vitest'
test('passes before worker crashes', () => {})
test('worker crashes', async () => {
  // Let Vitest publish the preceding result before killing the worker. Its
  // process.exit wrapper throws instead of exercising genuine worker death.
  await new Promise(resolve => setTimeout(resolve, 200))
  if (process.env.OTEL_TEST_POOL === 'threads') process.reallyExit(1)
  else process.kill(process.pid, 'SIGKILL')
})
test('never reached', () => {})
