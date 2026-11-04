# Changelog

## 1.13.0-beta.3

- Jest selects OpenTelemetry 2.x SDK-line instrumentations declared in the Jest
  rootDir's package.json or its ancestors (including workspace roots), plus a
  resolvable auto-instrumentations-node bundle's dependencies, including non-hoisted
  packages. Hoisted transitive packages alone no longer opt a project in.
  Like auto-instrumentations-node,
  `fs` and `host-metrics` are off by default unless named in
  `OTEL_NODE_ENABLED_INSTRUMENTATIONS`; `OTEL_NODE_DISABLED_INSTRUMENTATIONS`
  always takes precedence.
- Shared Jest workers respect each project's instrumentation selection regardless
  of project order, including HTTP/fetch spans and trace headers, without
  duplicating built-in patches or interrupting already-started requests.
  Inactive HTTP/HTTPS calls bypass instrumentation before argument normalization,
  preserving Node's URL validation for opted-out and uninstrumented files.
- Reapply upstream HTTP/HTTPS patches at each managed file's first activation
  when interceptors such as nock have restored `request` or `get`.
- Skip package patches with one warning when their OpenTelemetry API is mocked
  or incomplete, preserving application behavior and test-result export.
- Preserve unchanged getter-only module exports instead of assigning them back
  into Jest's registry, fixing default Fastify apps using `abstract-logging`.
  Add a real Fastify Jest/OTLP regression and document additional checked
  integrations, partial incoming propagation and explicit-setup-only packages.
- Make `pino`, `winston` and `bunyan` opt-in via
  `OTEL_NODE_ENABLED_INSTRUMENTATIONS` so default collection does not change log
  records. Metrics-only `runtime-node` and browser-only `fetch`, `xml-http-request`,
  `document-load`, `user-interaction` and `long-task` are also opt-in.
  Keep payload-changing `aws-sdk`, `kafkajs`, `amqplib` and low-level `net`/`dns`
  opt-in too. Document span counts, including Express 5/router overlap, and
  message-context injection opt-outs. The existing debug switch logs the selected
  instrumentation names once per worker at exit.
- Count children started under a completed file's trace after teardown in the
  existing dropped-children warning; already-started children can still finish
  and export under their original test.
