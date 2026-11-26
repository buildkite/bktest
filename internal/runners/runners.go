// Package runners lists bktest's built-in runners. Each is a runner plugin
// handler in a subpackage, which bktest calls in its own process and which
// plugins/<runner>/go also builds into an external plugin.
package runners

import (
	"github.com/buildkite/bktest/v3/internal/runners/cucumber"
	"github.com/buildkite/bktest/v3/internal/runners/custom"
	"github.com/buildkite/bktest/v3/internal/runners/cypress"
	"github.com/buildkite/bktest/v3/internal/runners/gotest"
	"github.com/buildkite/bktest/v3/internal/runners/jest"
	"github.com/buildkite/bktest/v3/internal/runners/playwright"
	"github.com/buildkite/bktest/v3/internal/runners/pytest"
	"github.com/buildkite/bktest/v3/internal/runners/rspec"
	"github.com/buildkite/bktest/v3/internal/runners/vitest"
	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

// Builtin maps each built-in runner's name, as given to --test-runner, to
// its handler constructor.
var Builtin = map[string]func() runnerplugin.Handler{
	"cucumber":   cucumber.New,
	"custom":     custom.New,
	"cypress":    cypress.New,
	"gotest":     gotest.New,
	"jest":       jest.New,
	"playwright": playwright.New,
	"pytest":     pytest.New,
	"rspec":      rspec.New,
	"vitest":     vitest.New,
}
