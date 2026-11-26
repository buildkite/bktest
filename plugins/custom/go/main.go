// Command bktest-runner-custom is bktest's built-in custom runner as an external
// runner plugin. Build it with plugins/build.sh.
package main

import (
	"github.com/buildkite/bktest/v3/internal/runners/custom"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func main() { runnerplugin.Serve(custom.New()) }
