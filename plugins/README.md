# Runner plugins

Every built-in bktec test runner, as external [runner plugins](../docs/runner-plugins.md) in two languages:

```
plugins/
  <runner>/
    go/     plugin.json, main.go    The built-in runner's own code, built as an executable
    bash/   plugin.json, plugin.bash    An independent implementation in Bash and jq
  lib/runner-plugin.bash    The Bash SDK the Bash plugins share
  build.sh                  Builds the Go plugins
```

`<runner>` is one of `rspec`, `jest`, `vitest`, `cypress`, `playwright`, `pytest`, `gotest`, `cucumber` and `custom`.

You don't need these to use bktec: the same runners are built in. They exist as working references for writing your own plugin, and to check that the protocol is enough to implement every runner without bktec's internals.

## Using one

Load a plugin from its subdirectory:

```sh
bktec run --runner-plugin plugins/rspec/bash
```

The Go plugins need building first, which writes `plugins/<runner>/go/bktest-runner-<runner>`:

```sh
plugins/build.sh          # all of them
plugins/build.sh rspec    # or just some
bktec run --runner-plugin plugins/rspec/go
```

To use a plugin outside this repository, copy its directory. A Bash plugin also needs `lib/runner-plugin.bash` two directories above it, or `BKTEST_RUNNER_PLUGIN_LIB` set to that file's path.

## Requirements

- Go plugins: nothing beyond what the test framework needs.
- Bash plugins: Bash 4.4 or later and jq 1.6 or later. The plugins that read JUnit XML (`pytest`, `gotest` and `custom` when the result path ends in `.xml`) also need `python3`.

## How they are tested

`TestPluginParity` in [`internal/runner`](../internal/runner/plugin_parity_test.go) runs the same scenarios, with the real test frameworks, against the built-in runner, the Go plugin and the Bash plugin of each runner, and requires identical results. `TestRunnerPluginsEndToEnd` in [`cmd/bktest`](../cmd/bktest/runner_plugin_e2e_test.go) runs a release build of bktec with each kind.

To work on one variant:

```sh
BKTEST_PARITY_VARIANTS=built-in,bash go test ./internal/runner -run 'TestPluginParity/rspec'
```
