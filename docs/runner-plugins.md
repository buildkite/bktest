# Runner plugins (experimental)

A runner plugin teaches bktest a test framework without changing bktest. Each
built-in runner (RSpec, Cucumber, Jest, Vitest, Playwright, Cypress, pytest and
go test) also exists as two plugins in [`plugins/`](../plugins): one written in
Bash and one compiled from Go. Both implement the same protocol, and both keep
the built-in runner's test discovery, identities, retries and results.

```sh
bktest run --plugin plugins/rspec/bash
bktest run --plugin plugins/rspec/go   # after plugins/build.sh
```

`--plugin` (or `BUILDKITE_TEST_ENGINE_PLUGIN`) takes a plugin directory. The
runner name defaults to the plugin's, so `--test-runner` isn't needed, and
bktest sends the same runner name to Test Engine as the built-in runner. Every
other runner setting, such as `--test-command`, `--retry-command`,
`--result-path` and `--test-file-pattern`, works as it does for that runner.

Plugins run as trusted job code with the job's permissions: a plugin
directory is not a sandbox. Only `bktest run` and `bktest plan` support
plugins; the Test Scheduler pool commands refuse them.

## How a plugin works

bktest keeps everything that isn't about the framework: Test Engine requests,
plans and fallback, retries and their budget, muting, the summary and the job's
exit status. It also runs the test command itself, so output streaming and
signal forwarding behave the same for every plugin. The plugin answers the
framework questions: which targets and examples exist, which command runs a set
of tests, and what that command's exit status and report mean.

```
bktest                                   plugin (one process per bktest run)
  start plugin ─────────────────────────▶
  initialize(config) ───────────────────▶ apply defaults
                    ◀─────── name, features, result path and format
  discover ─────────────────────────────▶ glob files, `go list`, ...
  examples(files)  (split by example) ──▶ dry run, --list, --collect-only
  fetch or create the test plan
  ┌ command(test cases, retry) ─────────▶ build argv
  │ run argv, stream output, forward signals
  │ interpret(test cases, exit code) ───▶ read the report
  │                 ◀─────── test results, errors, errored files, reruns
  └ retry failed tests, up to the retry count
  close stdin ──────────────────────────▶ exit
```

### Why one long-lived process over JSON-RPC

This is like a Terraform provider: bktest starts the plugin as a child
process, checks the protocol version in a handshake, keeps the process for the
whole run, and stops it at the end. A plugin is an executable, not a Go
`plugin` shared object, so it doesn't have to be built with bktest's toolchain,
runs on every platform bktest does, and can be written in any language.

Unlike Terraform, the transport is newline-delimited
[JSON-RPC 2.0](https://www.jsonrpc.org/specification) over the plugin's stdin
and stdout rather than gRPC, so a Bash script with `jq` speaks the same
protocol as a Go binary, without a bridge or a second transport. The process
lives for the whole run so a plugin can keep what it learns between attempts,
such as the exact Vitest test names it needs for retries.

bktest runs the test command, rather than the plugin, so that plugins don't
each have to forward signals, stream output, or tell a crashed test process
from a crashed plugin.

### Manifest

A plugin directory has a `plugin.json`:

```json
{
  "runner": "rspec",
  "protocol_version": 1,
  "command": ["./plugin.bash"]
}
```

- `runner` is the runner name bktest sends to Test Engine. Plugins that
  replace a built-in runner keep its name, so plans, timings and uploads keep
  matching the suite's history.
- `command` starts the plugin. A first element that starts with `./` is
  relative to the plugin directory; anything else is run as is, such as an
  executable on `PATH`.

The plugin starts in bktest's working directory with bktest's environment,
plus `BKTEST_RUNNER_PLUGIN_PROTOCOL=1`. A plugin started without that variable
explains itself and exits. Its stderr goes to bktest's stderr; its stdout is
the protocol channel, so anything else it runs, such as a dry run, must write
elsewhere.

### Methods

bktest sends one request at a time and waits for its response. Types are in
[`pkg/runnerplugin`](../pkg/runnerplugin/protocol.go).

| Method | Params | Result |
| --- | --- | --- |
| `initialize` | `protocol_version`, `config`: the runner flags the user set (`test_command`, `retry_test_command`, `test_file_pattern`, `test_file_exclude_pattern`, `result_path`, `location_prefix`, `tag_filters`, `selector_list_path`, `debug`) | `protocol_version`, `name` (shown in output), `features`, `result_format` (to upload `result_path` with, or empty), effective `result_path` and `location_prefix` |
| `discover` | | `targets`: files, packages or other selectors to plan |
| `examples` | `files` | `test_cases` in those files, for split by example and tag filters |
| `command` | `test_cases`, `retry` | `argv`, and optionally `stdout_path`: a file bktest also writes the command's stdout to |
| `interpret` | `test_cases`, `retry`, `exit_code` (null if the command didn't exit normally), `error` | see below |

`interpret` results:

- `tests`: each test case with a `status` (`passed`, `failed`, `skipped`,
  `not_run`, `unknown`) and an optional `key` that identifies it across
  attempts. The default key is scope, name and path. RSpec uses its example
  ID, because descriptions can repeat.
- `error`: an error outside the tests that retrying can't fix, such as a build
  failure. It ends the run with an error.
- `errored_files`: files that failed outside their tests, such as files that
  didn't load. They error the run, but other failed tests are still retried.
- `rerun`: test cases to run again in the same attempt, such as the files
  Playwright skipped because another file failed to load.
- `failed_tests_message`: fails the attempt with this message if the command
  exited 0 but the run has failed tests, as Vitest can.
- `messages`: lines bktest prints before recording the results.

The attempt's error is the command's own, as for built-in runners: a plugin
reports what happened, and bktest decides what to retry and how the job ends.

A plugin that fails a request returns a JSON-RPC error; bktest shows its
message and stops. A plugin that exits early fails the request in progress.

## Writing a plugin

### Go

Implement [`runnerplugin.Plugin`](../pkg/runnerplugin/serve.go) and call
`runnerplugin.Serve`. The package also has the file discovery the built-in
runners use, and [`junit`](../pkg/runnerplugin/junit) parses JUnit XML. The Go
plugins in `plugins/*/go` import nothing else from bktest, so they can live in
another module. Build them with `plugins/build.sh`.

### Bash

Source [`plugins/lib/runner-plugin.bash`](../plugins/lib/runner-plugin.bash),
define `plugin_initialize`, `plugin_discover`, `plugin_examples`,
`plugin_command` and `plugin_interpret`, and call `bk_serve`. Each handler
reads `$BK_PARAMS` and prints its JSON result; `bk_fail` fails a request. The
library has helpers for globbing, splitting and splicing commands, and reading
JUnit XML. It needs Bash 4 and `jq`; JUnit XML needs `python3`. To share a Bash
plugin outside this repository, copy the library with it and set
`BKTEST_RUNNER_PLUGIN_LIB`, or adjust its `source` line.

## Parity and limitations

`TestPluginParity` in `internal/runner` runs the same scenarios with each
built-in runner and both of its plugins, using the real frameworks in
`internal/runner/testdata`, and requires the same features, targets, examples,
commands and, for each attempt, the same error, run status and test results.
It covers RSpec example IDs and errors outside examples, Cucumber scenario
statuses, Jest runtime errors and name-pattern retries, Vitest retries by exact
names and exit 0 with failures, Playwright projects, skipped and not-run tests,
load errors with reruns and other errors, pytest collector JSON and JUnit XML
with collection errors, go test with gotestsum JSON, `go test -json` captured
from stdout, JUnit XML and build failures, and Cypress commands and exit
status.

Known differences and gaps:

- Plugins print their own wording for unreadable reports, debug logs and
  pytest's setup errors; the results are the same.
- Bash plugins glob with Bash's `globstar`, not bktest's glob library. Default
  patterns discover the same files, including in hidden directories such as a
  `.venv`, but Bash doesn't follow symlinked directories, and unusual patterns
  and exclude patterns can differ. They resolve report paths relative to the working
  directory only for files under it.
- A plugin error during `initialize`, such as pytest's `--tag-filters` without
  the collector, exits bktest with status 16 instead of the built-in runner's 1.
- Pools (`bktest pool ...`) don't support plugins yet. Their report parsing and
  persistent runners still use the built-in runners.
- The built-in runners are still in bktest, both as the default and as the
  reference for parity. Removing them depends on shipping plugins with bktest.
- Windows is untested. Bash plugins need Bash and `jq`.
