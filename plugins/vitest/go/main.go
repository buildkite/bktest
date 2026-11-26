// Command bktest-runner-vitest is bktest's built-in vitest runner as an external
// runner plugin. Build it with plugins/build.sh.
package main

import (
	"github.com/buildkite/bktest/v3/internal/runners/vitest"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func main() { runnerplugin.Serve(vitest.New()) }
