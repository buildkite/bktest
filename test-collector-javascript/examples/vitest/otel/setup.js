import { beforeEach } from 'vitest'
import { trace } from '@opentelemetry/api'
beforeEach(() => trace.getTracer('setup').startSpan('setup-hook').end())
