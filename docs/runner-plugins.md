# Runner plugins

> [!NOTE]
> Runner plugins are experimental. The protocol is version 1 and may change before it is declared stable.

A runner plugin teaches bktec how to work with a test framework: its default commands and file patterns, which tests exist, which command runs a set of tests, and what a finished command's report means. bktec keeps everything else: talking to Test Engine, plans, retries, muting, starting and signalling the test command, and the job's exit status.

Every built-in test runner is a runner plugin that bktec calls in its own process. An external plugin is an executable that answers the same operations, so you can replace a built-in runner or add your own without changing bktec:

```sh
bktec run --runner-plugin path/to/plugin
```

The [`plugins/`](../plugins) directory has every built-in runner twice more, as external plugins: once as a Go program that shares the built-in runner's code, and once as a Bash script that implements the protocol on its own. Use them as references for either language.

## Using a plugin

`--runner-plugin` (or `BUILDKITE_TEST_ENGINE_RUNNER_PLUGIN`) takes a plugin directory, or the path of its `plugin.json`. Nothing else loads a plugin: `--test-runner rspec` on its own always means the built-in runner, whatever files or directories the working directory has.

The plugin's `plugin.json` names the test runner it implements, so `--test-runner` can be left out. If both are given they must agree.

All other options keep their meaning. For example `--test-command`, `--test-file-pattern`, `--result-path` and `--retry-count` are passed to the plugin or handled by bktec exactly as for the built-in runner. `bktec plan` and `bktec pool` commands accept `--runner-plugin` too.

bktec validates options by runner name before it starts a plugin. A plugin that replaces a built-in runner keeps that runner's name, so the same rules apply to it: for example `rspec` needs `--result-path`, and `bktec pool run` accepts the same runners as without a plugin. Test Engine also receives that name with the plan request.

## Plugin directory

```
my-plugin/
  plugin.json
  my-plugin-executable
```

```json
{
  "runner": "rspec",
  "protocol_version": 1,
  "command": ["./my-plugin-executable"],
  "version": "1.2.0"
}
```

| Field | |
| --- | --- |
| `runner` | The test runner name bktec uses for validation and sends to Test Engine. |
| `protocol_version` | The protocol version the plugin implements. bktec refuses a plugin with another version. |
| `command` | The command that runs the plugin. A first element starting with `./` is relative to the plugin directory; anything else is looked up on `PATH`. bktec appends the operation. |
| `version` | Optional. The plugin's own version, shown in bktec's output. |

bktec does not build, download or update plugins. Build a compiled plugin before using it, and ship the directory however you ship other CI tooling, such as in the repository or the build image.

## Protocol

bktec starts the plugin once for each operation:

```
<command...> <operation>
```

- The request is a JSON object on stdin, and the response is a JSON object on stdout. stdout must carry nothing else.
- stderr goes straight to bktec's stderr. Use it for logs.
- The plugin runs in bktec's working directory, with bktec's environment.
- An operation fails by exiting with a non-zero status after writing `{"error": "message"}` to stdout. bktec shows the message and stops.
- Every request and response has `"protocol_version": 1`.

Because each operation is a new process, a plugin keeps nothing in memory between operations. Every request carries:

- `config`: the configuration that `configure` resolved.
- `state_dir`: a directory that exists for the whole bktec command and is removed when it ends. Keep anything a later operation needs there, such as the exact test names Vitest needs to retry tests.

bktec doesn't limit how long an operation takes.

### Operations

| Operation | Required | Purpose |
| --- | --- | --- |
| [`configure`](#configure) | Yes | Apply the plugin's defaults to the user's settings and describe the plugin. Called first, once. |
| [`prepare-attempt`](#prepare-attempt) | Yes | Return the command that runs a set of test cases. |
| [`discover-targets`](#discover-targets) | No | Return the files, packages or other targets to plan, when they aren't the files matching the file pattern. |
| [`discover-examples`](#discover-examples) | No | Return the individual tests in a set of files, for split by example and tag filters. |
| [`interpret-attempt`](#interpret-attempt) | No | Turn a finished command and its report into test outcomes. |
| [`parse-batch-report`](#parse-batch-report) | No | Read a report for Test Scheduler pools. |

A plugin lists the optional operations it implements in its `configure` response. bktec never calls the others.

### Common types

`config`:

| Field | From |
| --- | --- |
| `test_command` | `--test-command` |
| `retry_test_command` | `--retry-command` |
| `test_file_pattern` | `--test-file-pattern` |
| `test_file_exclude_pattern` | `--test-file-exclude-pattern` |
| `result_path` | `--result-path` |
| `location_prefix` | `--location-prefix` |
| `tag_filters` | `--tag-filters` |
| `selector_list_path` | `--selector-file` |
| `persistent_runner` | `true` under `bktec pool run -- <runner>`, where a persistent runner runs the tests instead of the test command |
| `debug` | `true` under `--debug` |

Unset settings are absent.

A test case:

| Field | |
| --- | --- |
| `path` | What the framework can run: a file, or an example such as `spec/a_spec.rb[1:2]`. |
| `format` | `file`, `example` or `selector`. |
| `value` | For a `selector` test case, the selector to run instead of `path`. |
| `scope`, `name` | The test's group and name, the way the Buildkite test collector for the framework reports them. Muting matches tests on these. |
| `identifier` | The framework's own ID for the test, if it has one. |

### `configure`

```json
{"protocol_version": 1, "state_dir": "/tmp/bktest-runner-123", "config": {"result_path": "tmp/rspec.json"}}
```

```json
{
  "protocol_version": 1,
  "name": "RSpec",
  "config": {
    "test_command": "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}",
    "retry_test_command": "bundle exec rspec --format progress --format json --out {{resultPath}} {{testExamples}}",
    "test_file_pattern": "spec/**/*_spec.rb",
    "result_path": "tmp/rspec.json",
    "location_prefix": "./"
  },
  "features": {"split_by_file": true, "split_by_example": true, "split_by_selector": true, "filter_test_files": true, "auto_retry": true, "mute": true, "skip": true},
  "result_format": "rspec-json",
  "operations": ["discover-examples", "interpret-attempt", "parse-batch-report"]
}
```

| Field | |
| --- | --- |
| `name` | Shown in bktec's output. |
| `config` | The request's configuration with the plugin's defaults filled in. bktec uses it from here on and sends it with every request. |
| `features` | The bktec features the runner supports: `split_by_file`, `split_by_example`, `split_by_selector`, `filter_test_files`, `filter_test_by_tag`, `auto_retry`, `mute`, `skip`. Absent means `false`. A plugin with `split_by_example` must implement `discover-examples`. |
| `result_format` | The format bktec uploads the report at `result_path` with when result uploads are on. Leave it out if bktec shouldn't upload it. |
| `batch_report_format` | The format `parse-batch-report` reads the report at `result_path` with, when it differs from `result_format`. |
| `operations` | The optional operations the plugin implements. |

Fail the operation to reject a configuration, such as a missing required setting.

### `prepare-attempt`

```json
{"protocol_version": 1, "state_dir": "...", "config": {}, "retry": false, "test_cases": [{"path": "spec/a_spec.rb"}, {"path": "spec/b_spec.rb[1:2]", "format": "example"}]}
```

```json
{"protocol_version": 1, "argv": ["bundle", "exec", "rspec", "--format", "json", "--out", "tmp/rspec.json", "spec/a_spec.rb", "spec/b_spec.rb[1:2]"]}
```

`retry` is `true` when the test cases are failed tests being retried.

bktec runs `argv` itself, without a shell, with the working directory and environment of bktec plus the variables it gives every test process, such as `BUILDKITE_ANALYTICS_TOKEN`. It streams the output and forwards SIGINT, SIGTERM, SIGHUP and SIGQUIT to the command.

`stdout_path` in the response asks bktec to also write the command's stdout to that file, for a report that is printed instead of written, such as `go test -json`.

### `discover-targets`

Without this operation, bktec plans the files matching `config.test_file_pattern`, minus those matching `config.test_file_exclude_pattern` and anything under `node_modules`.

The request has only the common fields. The response has either the targets:

```json
{"protocol_version": 1, "targets": ["example.com/app/models", "example.com/app/controllers"]}
```

or file patterns for bktec to find files with, using its own matcher:

```json
{"protocol_version": 1, "pattern": "tests/**/*.bats", "exclude_pattern": "tests/slow/**"}
```

### `discover-examples`

```json
{"protocol_version": 1, "state_dir": "...", "config": {}, "files": ["spec/a_spec.rb"]}
```

```json
{"protocol_version": 1, "test_cases": [{"identifier": "./spec/a_spec.rb[1:1]", "path": "./spec/a_spec.rb[1:1]", "scope": "A", "name": "works"}]}
```

bktec calls it for the files Test Engine asks to split by example, and to apply tag filters. Each test case's `path` must be something `prepare-attempt` can run.

### `interpret-attempt`

bktec calls it after each command it ran for `prepare-attempt`. Without this operation only the command's exit status counts: a non-zero exit fails the job and nothing is retried.

```json
{"protocol_version": 1, "state_dir": "...", "config": {}, "retry": false, "test_cases": [{"path": "spec/a_spec.rb"}], "exit_code": 1, "error": "exit status 1"}
```

`exit_code` is `null` when the command didn't exit by itself, such as when it couldn't be started or was killed by a signal. `error` describes the failure and is absent on success.

```json
{
  "protocol_version": 1,
  "tests": [
    {"test_case": {"identifier": "./spec/a_spec.rb[1:1]", "path": "./spec/a_spec.rb[1:1]", "scope": "A", "name": "works"}, "status": "failed", "key": "spec/a_spec.rb[1:1]"}
  ]
}
```

| Field | |
| --- | --- |
| `tests` | The outcome of each test: `passed`, `failed`, `skipped`, `not_run` (expected to run but didn't) or `unknown`. bktec retries the failed ones with their `test_case`, so it must be something `prepare-attempt` can run. |
| `tests[].key` | Identifies the test across attempts, so a retry replaces the earlier result. The default is the test case's scope, name and path. |
| `messages` | Lines for bktec to print, such as why a report couldn't be read. |
| `run_error` | An error outside the tests that retrying can't fix, such as a build failure. It ends the run with an error. |
| `errored_files` | Files that failed outside their tests, such as files that didn't load. They fail the run, but other failed tests are still retried. |
| `rerun` | Test cases for bktec to run again within this attempt, such as the files Playwright didn't run because another file failed to load. |
| `failed_tests_message` | Fail the attempt with this message if the command exited 0 while tests recorded in this run are still failed. |

The attempt's own error is always the command's: a plugin can't turn a failed command into a passing attempt. If a report can't be read, return a `messages` entry rather than failing the operation, so the command's exit status still decides the job.

### `parse-batch-report`

`bktec pool run` without a persistent runner starts the test command for each lease, then needs every test in the report matched to the leased tests. This operation reads the report for that. It follows bktec's pool accounting closely and is the part of the protocol most likely to change.

```json
{"protocol_version": 1, "state_dir": "...", "config": {}, "format": "jest-json", "report_path": "/tmp/bktest-runner-123/batch-report-1"}
```

```json
{
  "protocol_version": 1,
  "tests": [{"test_case": {"path": "src/a.test.js", "scope": "a", "name": "works"}, "status": "passed", "selector": "src/a.test.js", "location": "src/a.test.js:3"}]
}
```

| Field | |
| --- | --- |
| `tests[].selector` | The leased selector the test belongs to, usually its file. |
| `tests[].location` | The test's `file:line`, when the report has it. |
| `errors_outside_tests` | The report has errors that belong to no test. |
| `errored_files` | Files that failed outside their tests. |
| `errored_files_stopped_run` | The errored files stopped every other test from running. |
| `unselected_tests` | The report can include tests the command wasn't asked to run, such as Playwright's setup projects. |

Fail the operation for a report that is incomplete or in another format.

## Writing a plugin in Go

Implement [`runnerplugin.Handler`](../pkg/runnerplugin/serve.go), plus the optional interfaces for the operations you need, and call `runnerplugin.Serve` from `main`:

```go
package main

import "github.com/buildkite/bktest/v3/pkg/runnerplugin"

type handler struct{}

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		c.TestCommand = "bin/test"
	}
	return runnerplugin.ConfigureResponse{Name: "My runner", Config: c, Features: runnerplugin.Features{SplitByFile: true}}, nil
}

func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	argv := append([]string{request.Config.TestCommand}, runnerplugin.Paths(request.TestCases)...)
	return runnerplugin.PrepareAttemptResponse{Argv: argv}, nil
}

func main() { runnerplugin.Serve(handler{}) }
```

`Serve` fills in `protocol_version` and `operations`. The built-in runners in [`internal/runners`](../internal/runners) are handlers like this one; [`plugins/<runner>/go`](../plugins) builds each of them into an external plugin with a three-line `main`.

## Writing a plugin in Bash

Source [`plugins/lib/runner-plugin.bash`](../plugins/lib/runner-plugin.bash), define a function for each operation, and end with `bk_main "$@"`. The SDK needs Bash 4.4 or later and jq 1.6 or later.

```bash
#!/usr/bin/env bash
source "$(dirname "${BASH_SOURCE[0]}")/lib/runner-plugin.bash"

plugin_configure() {
  bk_request '{
    name: "My runner",
    config: (.config | bk_default("test_command"; "bin/test")),
    features: {split_by_file: true}
  }'
}

plugin_prepare_attempt() {
  bk_request '{argv: ((bk_test_command | bk_words) + bk_paths)}'
}

bk_main "$@"
```

`bk_request` runs a jq filter over the request. Requests can list thousands of tests, more than fit in a command's arguments, so build the response from the request with jq instead of copying it into shell variables. The SDK's comments describe its helpers.

## Trying an operation

`bktec tools runner-plugin <runner> <operation>` answers an operation with a built-in runner, which shows what bktec expects of a plugin for the same framework:

```sh
echo '{"protocol_version": 1, "state_dir": "/tmp", "config": {}}' | bktec tools runner-plugin rspec configure
```

An external plugin can be tried the same way: `./my-plugin-executable configure`.

## Limitations

- bktec doesn't limit how long a plugin operation runs, and a plugin can't set environment variables for the test command.
- Options are validated by runner name before the plugin starts. A plugin with a new runner name gets no runner-specific validation, and Test Engine must accept the name.
- External plugins are tested on Linux. They have not been tested on Windows.
