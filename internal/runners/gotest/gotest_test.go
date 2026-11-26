package gotest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
)

func TestGotestRun_GoJSONLEmptyResultFile(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "test-results.jsonl")
	err := os.WriteFile(resultPath, nil, 0o600)
	assert.NoError(t, err)

	var response runnerplugin.InterpretAttemptResponse
	err = parseGoJSONLResults(&response, resultPath)

	if err == nil || !strings.Contains(err.Error(), "no events found") {
		t.Errorf("parseGoJSONLResults() error = %v, want no events found", err)
	}
}

func TestGotestRun_GoJSONLFailureIsNotOverwrittenByLaterPass(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "test-results.jsonl")
	err := os.WriteFile(resultPath, []byte(`{"Action":"start","Package":"example.com/hello"}
{"Action":"fail","Package":"example.com/hello","Test":"TestFlaky"}
{"Action":"pass","Package":"example.com/hello","Test":"TestFlaky"}
{"Action":"fail","Package":"example.com/hello"}
`), 0o600)
	assert.NoError(t, err)

	var response runnerplugin.InterpretAttemptResponse
	err = parseGoJSONLResults(&response, resultPath)

	assert.NoError(t, err)
	// The package failed after the test's later pass, so the failure stands.
	want := []runnerplugin.TestResult{{
		TestCase: runnerplugin.TestCase{Format: runnerplugin.FormatExample, Scope: "example.com/hello", Name: "TestFlaky", Path: "example.com/hello"},
		Status:   runnerplugin.StatusFailed,
	}}
	if diff := cmp.Diff(response.Tests, want); diff != "" {
		t.Errorf("parseGoJSONLResults() tests diff (-got +want):\n%s", diff)
	}
}
