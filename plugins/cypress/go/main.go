// Command bktest-runner-cypress is bktest's built-in cypress runner as an external
// runner plugin. Build it with plugins/build.sh.
package main

import (
	"github.com/buildkite/bktest/v3/internal/runners/cypress"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

func main() { runnerplugin.Serve(cypress.New()) }
