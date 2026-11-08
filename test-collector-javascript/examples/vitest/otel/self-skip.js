import { test } from 'vitest'
import { tracer } from './app.js'

test('skips itself after doing work', (context) => {
  tracer.startSpan('work-before-skip').end()
  context.skip()
})
