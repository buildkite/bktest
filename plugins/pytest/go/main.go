// Command bktest-runner-pytest is bktest's built-in pytest runner as an external
// runner plugin. Build it with plugins/build.sh.
package main

import (
	"github.com/buildkite/bktest/v3/internal/runners/pytest"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func main() { runnerplugin.Serve(pytest.New()) }
