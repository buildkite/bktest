import { trace } from '@opentelemetry/api'
// Created at module import, before any test starts.
export const tracer = trace.getTracer('app')
