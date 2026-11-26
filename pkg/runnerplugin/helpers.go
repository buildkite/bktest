package runnerplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Paths returns what the framework runs for each test case: the selector
// value for selector test cases, otherwise the path.
func Paths(testCases []TestCase) []string {
	paths := make([]string, len(testCases))
	for i, tc := range testCases {
		if tc.Format == FormatSelector {
			paths[i] = tc.Value
		} else {
			paths[i] = tc.Path
		}
	}
	return paths
}

// TestCasesFromPaths returns a test case for each path.
func TestCasesFromPaths(paths []string) []TestCase {
	testCases := make([]TestCase, len(paths))
	for i, path := range paths {
		testCases[i] = TestCase{Path: path}
	}
	return testCases
}

// TestIdentifier is the default key bktest tracks a test with across
// attempts: its scope, name and path.
func TestIdentifier(tc TestCase) string {
	return tc.Scope + "/" + tc.Name + "/" + tc.Path
}

// Debugf logs to stderr when bktest runs with --debug.
func (c Config) Debugf(format string, args ...any) {
	if c.Debug {
		fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
	}
}

// ExitCodeOr returns the command's exit code, or def if it didn't exit
// normally, such as when it couldn't start.
func (r InterpretAttemptRequest) ExitCodeOr(def int) int {
	if r.ExitCode == nil {
		return def
	}
	return *r.ExitCode
}

// SaveState writes v as JSON to name in the state directory, for a later
// operation to read with LoadState.
func SaveState(stateDir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, name), data, 0o600)
}

// LoadState reads what SaveState wrote to name into v. It reports false,
// leaving v as it is, when nothing was saved.
func LoadState(stateDir, name string, v any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, name))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}
