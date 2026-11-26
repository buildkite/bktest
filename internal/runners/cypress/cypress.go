// Package cypress is the runner plugin for Cypress.
package cypress

import (
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type handler struct{}

// New returns the Cypress handler. It implements no optional operations:
// Cypress has no report bktest reads, so only its exit status counts.
func New() runnerplugin.Handler { return handler{} }

func (handler) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	c := request.Config
	if c.TestCommand == "" {
		c.TestCommand = "npx cypress run --spec {{testExamples}}"
	}
	if c.RetryTestCommand == "" {
		c.RetryTestCommand = c.TestCommand
	}
	if c.TestFilePattern == "" {
		c.TestFilePattern = "**/*.cy.{js,jsx,ts,tsx}"
	}
	return runnerplugin.ConfigureResponse{
		Name:   "Cypress",
		Config: c,
		Features: runnerplugin.Features{
			SplitByFile:     true,
			FilterTestFiles: true,
			SplitBySelector: true,
		},
	}, nil
}

// PrepareAttempt passes every spec file in one --spec argument, joined by
// commas.
func (handler) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	cmd := request.Config.TestCommand
	if request.Retry {
		cmd = request.Config.RetryTestCommand
	}
	words, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.PrepareAttemptResponse{}, err
	}
	specs := strings.Join(runnerplugin.Paths(request.TestCases), ",")
	if idx := slices.Index(words, "{{testExamples}}"); idx < 0 {
		words = append(words, "--spec", specs)
	} else {
		words[idx] = specs
	}
	return runnerplugin.PrepareAttemptResponse{Argv: words}, nil
}
