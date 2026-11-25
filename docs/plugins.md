# Runner Plugin System (Spike)

`bktest` delegates all test-framework integrations to external runner plugins. Every former built-in runner (`rspec`, `jest`, `vitest`, `cypress`, `playwright`, `pytest`, `gotest`, `cucumber`, `custom`) is implemented twice under `plugins/`:

- `plugins/go/<runner>/` — compiled external Go binaries (Terraform-provider style, built with `pkg/plugin`)
- `plugins/bash/<runner>/` — Bash executable scripts (sharing helpers in `plugins/bash/_lib/common.sh`)

## Why One-Shot Versioned JSON-over-Stdio Subcommands

Both compiled Go binaries and Bash scripts speak the exact same protocol (`protocol_version: 1`) without requiring Go `plugin` `.so` shared objects or maintaining separate transports (such as gRPC for Go and a bridge for Bash):

```text
bktest host                          plugin executable
  |                                        |
  |-- <plugin> configure < req.json ------>|  validate config, defaults, features, capabilities
  |<------------ resp.json ----------------|
  |                                        |
  |-- <plugin> discover-targets < req ---->|  file globs or `go list`
  |<------------ targets[] ----------------|
  |                                        |
  |-- <plugin> discover-examples < req --->|  optional (RSpec, Playwright, Pytest, Cucumber)
  |<------------ examples[] ---------------|
  |                                        |
  |-- <plugin> prepare-attempt < req ----->|  command, args, env, optional stdout_tee_path, state
  |<------------ command + args -----------|
  |                                        |
  |  [host runs command with signal        |
  |   forwarding, streaming stdio,         |
  |   optional stdout tee, retry loop]     |
  |                                        |
  |-- <plugin> interpret-attempt < req --->|  optional; parses report/JSONL, returns outcomes,
  |<------------ outcomes + state ---------|  rerun_test_cases, errored_files, run_error
  |                                        |
  |-- <plugin> parse-batch-report < req -->|  optional; Test Scheduler pool native reports
  |<------------ ParsedReport -------------|
```

Keeping test-command execution in the `bktest` host preserves signal forwarding, live stdout/stderr streaming, process-group cleanup, environment injection, and retry orchestration in one place while letting plugins own framework semantics:

- **RSpec**: explicit outcome `key` (example ID) so retries of generated descriptions match the original failure rather than duplicating keys.
- **Vitest**: host-carried opaque `state` (`retry_names` for `-t` regex retries) and `fail_if_failed_tests` so retries must pass to clear an initial failure.
- **Playwright**: distinguishes `not_run` vs `skipped`, detects file-load errors, and returns `rerun_test_cases` to re-run unaffected files when a single file fails to load.
- **Cypress**: comma-joins `--spec` paths and sets `capabilities.interpret_attempt: false` so exit status is the sole outcome.
- **Go (`gotest`)**: discovers packages via `go list` and uses `stdout_tee_path` so the host tees `go test -json` output to a file while streaming it live.
- **Pytest**: detects `buildkite-test-collector` at configure time, supports JUnit XML and JSON reports, parses exit-2 collection errors, and uses `use_command_error_as_run_error` for unparseable/fatal failures.
- **Cucumber**: runs dry-run example discovery and aggregates step statuses into scenario outcomes.
- **Custom**: expands `{{testExamples}}` as pre-split args and interprets optional JUnit XML or Test Engine JSON reports.
- **Test Scheduler pool**: `parse-batch-report` resolves plugins by native report format (`rspec-json`, `jest-json`, `playwright-json`, `pytest-json`, `pytest-junit`).

## Loading Plugins from Subdirectories

Plugins live in subdirectories containing a `plugin.json` manifest:

```json
{
  "protocol_version": 1,
  "name": "rspec",
  "display_name": "RSpec",
  "kind": "go",
  "entrypoint": "./bktest-plugin-rspec",
  "build": { "go_package": "." },
  "report_formats": ["rspec-json"]
}
```

`bktest` supports four ways to load a plugin:

1. **Direct subdirectory path** (`--plugin` or `BUILDKITE_TEST_ENGINE_PLUGIN`):
   ```bash
   bktest run --plugin ./plugins/bash/rspec
   bktest run --plugin ./plugins/go/rspec
   ```
2. **Terraform-style `root//subdir` reference**:
   ```bash
   bktest run --plugin ./plugins//go/pytest
   bktest run --plugin ./plugins//bash/pytest
   ```
3. **Recursive plugin directory scanning** (`--plugin-dir` / `BUILDKITE_TEST_ENGINE_PLUGIN_DIR`, optionally narrowed by `--plugin-variant go|bash` / `BUILDKITE_TEST_ENGINE_PLUGIN_VARIANT`):
   ```bash
   bktest run --test-runner vitest --plugin-dir ./plugins --plugin-variant bash
   bktest run --test-runner vitest --plugin-dir ./plugins --plugin-variant go
   ```
   When `--plugin-dir` is omitted, `bktest` searches `$BUILDKITE_TEST_ENGINE_PLUGIN_DIR`, `./plugins`, and `<executable-dir>/plugins` recursively.
4. **Standalone executable or `$PATH` lookup**:
   Passing an executable path to `--plugin` (or `--test-runner`) invokes it directly, and `--test-runner <name>` falls back to `bktest-plugin-<name>` on `$PATH`.

For Go plugins, if the compiled binary named by `entrypoint` already exists (for example after running `bin/build-plugins`), `bktest` executes it directly. Otherwise, if `build.go_package` is declared, `bktest` compiles and caches the binary automatically under `${XDG_CACHE_HOME:-~/.cache}/bktest/plugins/` keyed by a content fingerprint of the plugin directory.

## Building & Authoring Plugins

Pre-build all 9 Go plugins and validate all 9 Bash plugins:

```bash
bin/build-plugins --check
```

### Authoring a Go plugin

Import `github.com/buildkite/bktest/v3/pkg/plugin`, implement `plugin.Handler` (plus any of `ExampleDiscoverer`, `AttemptInterpreter`, `BatchReportParser`), and call `plugin.Serve(handler)` in `main()`.

### Authoring a Bash plugin

Create a directory with `plugin.json` pointing `entrypoint` to an executable `./plugin.sh`. Read the JSON request from `stdin`, switch on `$1` (`configure`, `discover-targets`, `discover-examples`, `prepare-attempt`, `interpret-attempt`, `parse-batch-report`), and print a JSON response with `"protocol_version": 1` to `stdout`. On error, write `{"protocol_version":1,"error":"..."}` to `stdout` and exit non-zero.

## Running Integration Examples

Run the Go + Bash dual-variant unit/fixture suite across all 9 runners:

```bash
go test -race ./internal/plugin/... ./pkg/plugin/... ./internal/runner/... ./internal/command/...
```

Run the end-to-end CLI integration test across all 9 runners × both Go and Bash variants:

```bash
go test -race -v -tags=integration ./internal/integration/... -run TestAllRunnerPluginsE2E
```

Or run the standalone demo script against the repository suites or a local `buildkite/bktest-examples` checkout:

```bash
examples/plugins/run-examples.sh
examples/plugins/run-examples.sh --examples-dir ../test-engine-client-examples
```
