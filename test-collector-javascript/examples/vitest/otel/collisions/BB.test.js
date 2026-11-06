import { test } from 'vitest'
import { trace } from '@opentelemetry/api'
test('second collision', () => trace.getTracer('app').startSpan('second child').end())
