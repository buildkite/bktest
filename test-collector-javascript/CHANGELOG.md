# Changelog

## 1.13.0-beta.4

- Set up OpenTelemetry with the reporter and runtime, and keep your own test environment. The beta `jest/environment` setup is gone: use your usual environment and add `runtime`.
- Installing no longer fails next to older OpenTelemetry packages, such as those from `@sentry/node` 8 or 9.
- Mocking `@opentelemetry/api`, or app code changing the active span, no longer changes test results.
- Limit test batches to 200 spans, preserve job links despite `OTEL_SPAN_*` settings, and flush test and child spans together.
- Type-check reporter options in `jest.config.ts` with `jest/reporter.d.ts`.
- Send selected instrumentation names to your OpenTelemetry `diag` logger.
- Keep beta.2's error stacks when no package instrumentation is selected.
- Simplify setup instructions, settings and troubleshooting.

## 1.13.0-beta.3

- Select project/ancestor-declared 2.x instrumentations and resolvable auto-instrumentations-node bundles, not incidental hoisted dependencies.
- Keep filesystem, log/message-changing, low-level, metrics-only and browser instrumentations opt-in; standard `OTEL_NODE_*` selection applies, with disabled names winning.
- Respect each project's selection in shared workers without duplicate HTTP/fetch spans or unwanted trace headers.
- Recover HTTP/HTTPS instrumentation after interceptors restore built-ins.
- Skip package patches with one warning for mocked/incomplete APIs; test results still export.
- Preserve unchanged getter-only module exports, including Fastify dependencies.
- Export already-started children after teardown; count new late descendants and unfinished children in dropped-span warnings.
- Document compatibility, span counts/billing and opt-outs; the existing debug switch reports selected names once per worker.
