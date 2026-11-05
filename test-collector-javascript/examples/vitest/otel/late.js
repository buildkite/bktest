import { test, afterAll } from 'vitest'
import { tracer } from './app.js'
test('late children', () => {
  tracer.startSpan('on-time').end()
  setTimeout(() => tracer.startSpan('too-late').end(), 30)
})
afterAll(() => new Promise(resolve => setTimeout(resolve, 100)))
