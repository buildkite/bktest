// Command bktest-runner-cucumber is bktest's built-in cucumber runner as an external
// runner plugin. Build it with plugins/build.sh.
package main

import (
	"github.com/buildkite/bktest/v3/internal/runners/cucumber"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func main() { runnerplugin.Serve(cucumber.New()) }
