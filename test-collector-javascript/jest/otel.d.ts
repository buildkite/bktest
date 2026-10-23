import type { SpanProcessor } from '@opentelemetry/sdk-trace-base'

/**
 * A span processor that forwards the children of each running test to
 * Buildkite Test Engine. Add it to the `spanProcessors` of a tracer provider
 * your suite constructs in a `setupFilesAfterEnv` file. It does nothing outside
 * the collector's Jest environment or when OpenTelemetry submission is off.
 */
export function createBuildkiteSpanProcessor(): SpanProcessor
