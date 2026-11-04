# Changelog

## 1.13.0-beta.3

- Jest discovers installed OpenTelemetry 2.x SDK-line instrumentations, including
  non-hoisted auto-instrumentations-node dependencies. Like auto-instrumentations-node,
  `fs` and `host-metrics` are off by default unless named in
  `OTEL_NODE_ENABLED_INSTRUMENTATIONS`; `OTEL_NODE_DISABLED_INSTRUMENTATIONS`
  always takes precedence.
- Shared Jest workers respect each project's instrumentation selection regardless
  of project order, including HTTP/fetch spans and trace headers, without
  duplicating built-in patches or interrupting already-started requests.
