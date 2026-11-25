// Command bktest-runner-cypress is the Go runner plugin for Cypress.
package main

import (
	"errors"
	"slices"
	"strings"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/kballard/go-shellquote"
)

type cypress struct {
	cfg runnerplugin.Config
	log runnerplugin.Logger
}

func main() { runnerplugin.Serve(&cypress{}) }

func (c *cypress) Initialize(params runnerplugin.InitializeParams) (runnerplugin.InitializeResult, error) {
	c.cfg = params.Config
	c.log.Enabled = c.cfg.Debug
	if c.cfg.TestCommand == "" {
		c.cfg.TestCommand = "npx cypress run --spec {{testExamples}}"
	}
	if c.cfg.RetryTestCommand == "" {
		c.cfg.RetryTestCommand = c.cfg.TestCommand
	}
	if c.cfg.TestFilePattern == "" {
		c.cfg.TestFilePattern = "**/*.cy.{js,jsx,ts,tsx}"
	}
	return runnerplugin.InitializeResult{
		ProtocolVersion: runnerplugin.ProtocolVersion,
		Name:            "Cypress",
		Features: runnerplugin.Features{
			SplitByFile:     true,
			FilterTestFiles: true,
			SplitBySelector: true,
		},
		ResultPath:     c.cfg.ResultPath,
		LocationPrefix: c.cfg.LocationPrefix,
	}, nil
}

func (c *cypress) Discover() (runnerplugin.DiscoverResult, error) {
	c.log.Printf("Discovering test files with include pattern: %s exclude pattern: %s", c.cfg.TestFilePattern, c.cfg.TestFileExcludePattern)
	files, err := runnerplugin.DiscoverFiles(c.cfg.TestFilePattern, c.cfg.TestFileExcludePattern)
	return runnerplugin.DiscoverResult{Targets: files}, err
}

func (c *cypress) Examples(runnerplugin.ExamplesParams) (runnerplugin.ExamplesResult, error) {
	return runnerplugin.ExamplesResult{}, errors.New("cypress does not support split by example")
}

// Command passes every spec file to one --spec argument, joined by commas.
func (c *cypress) Command(params runnerplugin.CommandParams) (runnerplugin.CommandResult, error) {
	cmd := c.cfg.TestCommand
	if params.Retry {
		cmd = c.cfg.RetryTestCommand
	}
	words, err := shellquote.Split(cmd)
	if err != nil {
		return runnerplugin.CommandResult{}, err
	}
	specs := strings.Join(runnerplugin.Paths(params.TestCases), ",")
	if idx := slices.Index(words, "{{testExamples}}"); idx < 0 {
		words = append(words, "--spec", specs)
	} else {
		words[idx] = specs
	}
	return runnerplugin.CommandResult{Argv: words}, nil
}

// Interpret reports nothing: Cypress has no report, so only its exit status
// counts and failed tests aren't retried.
func (c *cypress) Interpret(runnerplugin.InterpretParams) (runnerplugin.InterpretResult, error) {
	return runnerplugin.InterpretResult{}, nil
}
