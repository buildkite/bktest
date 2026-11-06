import { test } from 'vitest'
import { trace } from '@opentelemetry/api'
test('first collision', () => trace.getTracer('app').startSpan('first child').end())
