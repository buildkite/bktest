package runner

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
)

func TestCypressRun(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/cypress")

		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: "yarn cypress run --spec {{testExamples}}",
		})

		testCases := []plan.TestCase{
			{Path: "./cypress/e2e/passing_spec.cy.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := cypress.Run(result, testCases, false)

		if err != nil {
			t.Errorf("Cypress.Run(%v) error = %v", testCases, err)
		}

		if result.Status() != RunStatusUnknown {
			t.Errorf("Cypress.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
		}
	})
}

func TestCypressRun_TestFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/cypress")

		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: "yarn cypress run --spec {{testExamples}}",
		})

		testCases := []plan.TestCase{
			{Path: "./cypress/e2e/failing_spec.cy.js"},
			{Path: "./cypress/e2e/passing_spec.cy.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := cypress.Run(result, testCases, false)

		if result.Status() != RunStatusUnknown {
			t.Errorf("Cypress.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
		}

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
	})
}

func TestCypressRun_CommandFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: "yarn cypress run --json",
		})

		testCases := []plan.TestCase{}
		result := NewRunResult([]plan.TestCase{})
		err := cypress.Run(result, testCases, false)

		if result.Status() != RunStatusUnknown {
			t.Errorf("Cypress.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
		}

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
	})
}

func TestCypressRun_SignaledError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: "./testdata/segv.sh",
		})

		testCases := []plan.TestCase{
			{Path: "./doesnt-matter.cy.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := cypress.Run(result, testCases, false)

		if result.Status() != RunStatusUnknown {
			t.Errorf("Cypress.Run(%v) RunResult.Status = %v, want %v", testCases, result.Status(), RunStatusUnknown)
		}

		signalError := new(ProcessSignaledError)
		if !errors.As(err, &signalError) {
			t.Errorf("Cypress.Run(%v) error type = %T (%v), want *ErrProcessSignaled", testCases, err, err)
		}
		if signalError.Signal != syscall.SIGSEGV {
			t.Errorf("Cypress.Run(%v) signal = %d, want %d", testCases, syscall.SIGSEGV, signalError.Signal)
		}
	})
}

func TestCypressDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{})

		got, err := cypress.DiscoverTestTargets()
		if err != nil {
			t.Errorf("Cypress.DiscoverTestTargets() error = %v", err)
		}

		want := []string{
			"testdata/cypress/cypress/e2e/failing_spec.cy.js",
			"testdata/cypress/cypress/e2e/flaky_spec.cy.js",
			"testdata/cypress/cypress/e2e/passing_spec.cy.js",
		}

		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("Cypress.DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestCypressCommandNameAndArgs_WithInterpolationPlaceholder(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		testCases := []plan.TestCase{{Path: "cypress/e2e/passing_spec.cy.js"}, {Path: "cypress/e2e/flaky_spec.cy.js"}}
		testCommand := "cypress run --spec {{testExamples}}"

		cy := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: testCommand,
			ResultPath:  "cypress.json",
		})

		gotName, gotArgs, err := cy.CommandNameAndArgs(testCases, false)
		if err != nil {
			t.Errorf("commandNameAndArgs(%v, %q) error = %v", testCases, testCommand, err)
		}

		wantName := "cypress"
		wantArgs := []string{"run", "--spec", "cypress/e2e/passing_spec.cy.js,cypress/e2e/flaky_spec.cy.js"}

		if diff := cmp.Diff(gotName, wantName); diff != "" {
			t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
		}
		if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
			t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
		}
	})
}

func TestCypressCommandNameAndArgs_WithoutTestExamplesPlaceholder(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		testCases := []plan.TestCase{{Path: "cypress/e2e/passing_spec.cy.js"}, {Path: "cypress/e2e/flaky_spec.cy.js"}}
		testCommand := "cypress run"

		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: testCommand,
		})

		gotName, gotArgs, err := cypress.CommandNameAndArgs(testCases, false)
		if err != nil {
			t.Errorf("commandNameAndArgs(%v, %q) error = %v", testCases, testCommand, err)
		}

		wantName := "cypress"
		wantArgs := []string{"run", "--spec", "cypress/e2e/passing_spec.cy.js,cypress/e2e/flaky_spec.cy.js"}

		if diff := cmp.Diff(gotName, wantName); diff != "" {
			t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
		}
		if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
			t.Errorf("commandNameAndArgs(%v, %q) diff (-got +want):\n%s", testCases, testCommand, diff)
		}
	})
}

func TestCypressCommandNameAndArgs_InvalidTestCommand(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		testCases := []plan.TestCase{{Path: "cypress/e2e/passing_spec.cy.js"}, {Path: "cypress/e2e/flaky_spec.cy.js"}}
		testCommand := "cypress run --options '{{testExamples}}"

		cypress := loadVariantRunner(t, variant, "cypress", RunnerConfig{
			TestCommand: testCommand,
		})

		gotName, gotArgs, err := cypress.CommandNameAndArgs(testCases, false)

		wantName := ""
		wantArgs := []string{}

		if diff := cmp.Diff(gotName, wantName); diff != "" {
			t.Errorf("commandNameAndArgs() diff (-got +want):\n%s", diff)
		}
		if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
			t.Errorf("commandNameAndArgs() diff (-got +want):\n%s", diff)
		}
		if !errors.Is(err, shellquote.UnterminatedSingleQuoteError) {
			t.Errorf("commandNameAndArgs() error = %v, want %v", err, shellquote.UnterminatedSingleQuoteError)
		}
	})
}
