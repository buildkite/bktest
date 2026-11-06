import { beforeEach } from 'vitest'
import { trace } from '@opentelemetry/api'
beforeEach((context) => {
  if (context.task.name !== 'hook failure') trace.getTracer('setup').startSpan('setup-hook').end()
})
