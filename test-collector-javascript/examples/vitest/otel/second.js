import { test, beforeAll } from 'vitest'
import { tracer } from './app.js'
beforeAll(() => tracer.startSpan('second-before-all-not-a-child').end())
test('second file', () => tracer.startSpan('second-body').end())
