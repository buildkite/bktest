# Changelog

## 1.10.0

- Promote `1.10.0b1` to a stable package release with no collector runtime
  changes. OpenTelemetry submission remains experimental, opt-in and disabled
  by default.

## 1.10.0b1 — 2026-11-05

- Add experimental, opt-in OpenTelemetry submission for pytest, including test
  results, child spans and populated setup/body/teardown phases. Install
  `buildkite-test-collector[otel]==1.10.0b1` and set
  `BUILDKITE_ANALYTICS_OTEL_ENABLED=true`; see the
  [OpenTelemetry guide](docs/opentelemetry.md) for setup and limitations.
- Fix result finalization for teardown failures and pytest-rerunfailures, preserve
  the original failure when teardown also fails, and use 1-based source lines.
- Support non-empty `BUILDKITE_ANALYTICS_*` run metadata overrides for JSON and
  OpenTelemetry submission.
- Include the release script and pipeline in source distributions so the bundled
  release-gate tests can run, and pin OTel quick-start instructions to this beta.

## 1.9.1 — 2026-10-20

- Require certifi >= 2026.10.19 as defence in depth against the eMudhra
  intermediate CA key compromise ([CVE-2026-111943](https://emudhra.we-have-a-problem.com/)).
  Existing releases are not yanked; users should upgrade to receive the new
  minimum trust-store version.
- Update the locked trust store and pin CI to uv 0.13.4. Refresh system CA
  certificates and require patched pip and certifi for package builds and
  publishing, including twine's isolated environment.
