package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/buildkite/bktest/v3/internal/plan"
	"github.com/google/go-cmp/cmp"
	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaywrightRun(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/playwright")

		playwright := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: "yarn run playwright test",
			ResultPath:  "test-results/results.json",
		})

		testCases := []plan.TestCase{
			{Path: "./testdata/playwright/tests/example.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := playwright.Run(result, testCases, false)

		assert.NoError(t, err)
		assert.Len(t, result.tests, 5)
		assert.Equal(t, RunStatusPassed, result.Status())
	})
}

func TestPlaywrightRun_TestFailed(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/playwright")

		playwright := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			ResultPath: "test-results/results.json",
		})
		t.Cleanup(func() { os.Remove(playwright.ResultFilePath()) })

		testCases := []plan.TestCase{
			{Path: "./tests/failed.spec.js"},
		}
		result := NewRunResult([]plan.TestCase{})
		err := playwright.Run(result, testCases, false)

		wantFailedTests := []plan.TestCase{
			{Scope: " chromium failed.spec.js test group failed", Path: "failed.spec.js:5", Name: "failed"},
			{Scope: " firefox failed.spec.js test group failed", Path: "failed.spec.js:5", Name: "failed"},
			{Scope: " chromium failed.spec.js timed out", Path: "failed.spec.js:14", Name: "timed out"},
			{Scope: " firefox failed.spec.js timed out", Path: "failed.spec.js:14", Name: "timed out"},
		}

		exitError := new(exec.ExitError)
		assert.ErrorAs(t, err, &exitError)
		assert.Equal(t, RunStatusFailed, result.Status())

		sorter := cmp.Transformer("Sort", func(in []plan.TestCase) []plan.TestCase {
			out := append([]plan.TestCase(nil), in...)
			slices.SortFunc(out, func(a, b plan.TestCase) int {
				return strings.Compare(a.Scope+"/"+a.Name, b.Scope+"/"+b.Name)
			})
			return out
		})
		if diff := cmp.Diff(result.FailedTests(), wantFailedTests, sorter); diff != "" {
			t.Errorf("FailedTests() diff (-got +want):\n%s", diff)
		}
	})
}

func TestPlaywrightRun_NotRunVsSkippedAndLoadErrors(t *testing.T) {
	// Deterministic fixture-backed test exercising Playwright's subtle accounting
	// semantics in BOTH Go and Bash plugins without requiring browsers:
	// 1. Failure-induced skip (status=skipped, expectedStatus=passed) -> not_run
	// 2. Intentional skip (status=skipped, expectedStatus=skipped) -> skipped
	// 3. File-load error in requested file -> erroredFiles + immediate rerun of remaining files
	// 4. Error outside requested files -> fatal run error (non-retryable)
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		dir := t.TempDir()
		changeCwd(t, dir)

		// Case A: intentional skip vs not_run
		reportA := filepath.Join(dir, "report-a.json")
		require.NoError(t, os.WriteFile(reportA, []byte(`{
			"suites": [{
				"title": "serial.spec.js",
				"specs": [
					{"file":"serial.spec.js","line":3,"title":"intentional skip","ok":true,"tests":[{"projectName":"chromium","status":"skipped","expectedStatus":"skipped"}]},
					{"file":"serial.spec.js","line":7,"title":"skipped after failure","ok":true,"tests":[{"projectName":"chromium","status":"skipped","expectedStatus":"passed"}]}
				]
			}]
		}`), 0o600))

		pwA := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: "true {{testExamples}}",
			ResultPath:  reportA,
		})
		resA := NewRunResult(nil)
		require.NoError(t, pwA.Run(resA, []plan.TestCase{{Path: "serial.spec.js"}}, false))
		assert.Equal(t, RunStatusFailed, resA.Status())
		assert.Len(t, resA.SkippedTests(), 1)
		assert.Equal(t, "intentional skip", resA.SkippedTests()[0].Name)
		assert.Len(t, resA.NotRunTests(), 1)
		assert.Equal(t, "skipped after failure", resA.NotRunTests()[0].Name)

		// Case B: file-load error reruns remaining files
		reportB := filepath.Join(dir, "report-b.json")
		errFileAbs := filepath.Join(dir, "tests", "error.spec.js")
		scriptB := filepath.Join(dir, "pw-sim.sh")
		require.NoError(t, os.WriteFile(scriptB, []byte(fmt.Sprintf(`#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == *"error.spec.js"* ]]; then
    cat > %q <<'JSON'
{"suites":[],"errors":[{"message":"SyntaxError","location":{"file":%q}}]}
JSON
    exit 1
  fi
done
cat > %q <<'JSON'
{"suites":[{"title":"example.spec.js","specs":[{"file":"example.spec.js","line":4,"title":"passes","ok":true,"tests":[{"projectName":"chromium","status":"expected","expectedStatus":"passed"}]}]}],"errors":[]}
JSON
exit 0
`, reportB, errFileAbs, reportB)), 0o755))

		pwB := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: shellquote.Join(scriptB, "{{testExamples}}"),
			ResultPath:  reportB,
		})
		resB := NewRunResult(nil)
		err := pwB.Run(resB, []plan.TestCase{{Path: "./tests/example.spec.js"}, {Path: "./tests/error.spec.js"}}, false)
		assert.ErrorAs(t, err, new(*exec.ExitError))
		assert.Equal(t, RunStatusError, resB.Status())
		assert.True(t, resB.Retryable())
		assert.Equal(t, []string{"tests/error.spec.js"}, resB.ErroredFiles())
		assert.Equal(t, 1, resB.Statistics().PassedOnFirstRun)

		// Case C: error outside requested files (global setup error) -> fatal non-retryable error
		resC := NewRunResult(nil)
		err = pwB.Run(resC, []plan.TestCase{{Path: "./tests/example.spec.js"}, {Path: "error.spec.js"}}, false)
		assert.ErrorAs(t, err, new(*exec.ExitError))
		assert.Equal(t, RunStatusError, resC.Status())
		assert.False(t, resC.Retryable())
	})
}

func TestPlaywrightRun_SignaledError(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		playwright := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: "./testdata/segv.sh --outputFile {{resultPath}}",
		})

		result := NewRunResult([]plan.TestCase{})
		err := playwright.Run(result, []plan.TestCase{{Path: "./doesnt-matter.spec.js"}}, false)

		assert.Equal(t, RunStatusUnknown, result.Status())
		signalError := new(ProcessSignaledError)
		require.ErrorAs(t, err, &signalError)
		assert.Equal(t, syscall.SIGSEGV, signalError.Signal)
	})
}

func TestPlaywrightCommandNameAndArgs(t *testing.T) {
	testCases := []plan.TestCase{{Path: "tests/example.spec.js"}, {Path: "tests/failed.spec.js"}}
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		pw := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: "npx playwright test {{testExamples}}",
		})
		gotName, gotArgs, err := pw.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "npx", gotName)
		assert.Equal(t, []string{"playwright", "test", "tests/example.spec.js", "tests/failed.spec.js"}, gotArgs)

		pw2 := loadVariantRunner(t, variant, "playwright", RunnerConfig{
			TestCommand: "npx playwright test",
		})
		gotName, gotArgs, err = pw2.CommandNameAndArgs(testCases, false)
		require.NoError(t, err)
		assert.Equal(t, "npx", gotName)
		assert.Equal(t, []string{"playwright", "test", "tests/example.spec.js", "tests/failed.spec.js"}, gotArgs)
	})
}

func TestPlaywrightDiscoverTestTargets(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		changeCwd(t, "./testdata/playwright")
		playwright := loadVariantRunner(t, variant, "playwright", RunnerConfig{})

		got, err := playwright.DiscoverTestTargets()
		require.NoError(t, err)

		want := []string{
			"tests/error.spec.js",
			"tests/example.spec.js",
			"tests/failed.spec.js",
			"tests/skipped.spec.js",
		}
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("DiscoverTestTargets() diff (-got +want):\n%s", diff)
		}
	})
}

func TestPlaywrightGetExamples_EmptyFiles(t *testing.T) {
	forEachPluginVariant(t, func(t *testing.T, variant string) {
		playwright := loadVariantExampleRunner(t, variant, "playwright", RunnerConfig{})
		got, err := playwright.GetExamples([]string{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestParseNativePlaywrightReport(t *testing.T) {
	workDir, err := os.Getwd()
	require.NoError(t, err)
	rootDir := filepath.Join(workDir, "tests")

	forEachPluginVariant(t, func(t *testing.T, variant string) {
		data := []byte(fmt.Sprintf(`{
			"config": {"rootDir": %q},
			"suites": [{
				"title": "example.spec.js",
				"specs": [{
					"file": "example.spec.js",
					"line": 4,
					"title": "works",
					"ok": true,
					"tests": [
						{"projectName": "chromium", "status": "expected", "expectedStatus": "passed"},
						{"projectName": "firefox", "status": "skipped", "expectedStatus": "passed"}
					]
				}]
			}],
			"errors": []
		}`, rootDir))

		got, err := ParseNativeReportWithOptions("playwright-json", data, nil, variant)
		require.NoError(t, err)
		assert.True(t, got.UnselectedTests)
		assert.Equal(t, []string{"tests/example.spec.js"}, got.ErroredFiles)
		assert.Len(t, got.Tests, 2)
		assert.Equal(t, " chromium example.spec.js works", got.Tests[0].TestCase.Scope)
		assert.Equal(t, TestStatusPassed, got.Tests[0].Status)
		assert.Equal(t, " firefox example.spec.js works", got.Tests[1].TestCase.Scope)
		assert.Equal(t, TestStatusSkipped, got.Tests[1].Status)
	})
}
