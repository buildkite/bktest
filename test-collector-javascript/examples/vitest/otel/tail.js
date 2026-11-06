import { test } from 'vitest'
import { trace } from '@opentelemetry/api'
test('child completes in environment teardown', () => {
  const span = trace.getTracer('app').startSpan('accepted-child')
  setTimeout(() => {
    span.end()
    console.log('CHILD ENDED')
  }, 100)
})
