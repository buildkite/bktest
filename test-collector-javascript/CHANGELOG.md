# Changelog

## 1.13.0-beta.3

- Select project/ancestor-declared 2.x instrumentations and resolvable auto-instrumentations-node bundles, not incidental hoisted dependencies.
- Keep filesystem, log/message-changing, low-level, metrics-only and browser instrumentations opt-in; standard `OTEL_NODE_*` selection applies, with disabled names winning.
- Respect each project's selection in shared workers without duplicate HTTP/fetch spans or unwanted trace headers.
- Recover HTTP/HTTPS instrumentation after interceptors restore built-ins.
- Skip package patches with one warning for mocked/incomplete APIs; test results still export.
- Preserve unchanged getter-only module exports, including Fastify dependencies.
- Export already-started children after teardown; count new late descendants and unfinished children in dropped-span warnings.
- Document compatibility, span counts/billing and opt-outs; the existing debug switch reports selected names once per worker.
