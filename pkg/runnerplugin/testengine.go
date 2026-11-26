package runnerplugin

import (
	"encoding/json"
	"fmt"
	"os"
)

// TestEngineTest is a test result object in Test Engine's JSON format, as
// the Buildkite test collectors write. Attributes bktest doesn't need, such
// as history and failure_reason, are omitted.
// See https://buildkite.com/docs/test-engine/importing-json#json-test-results-data-reference-test-result-objects
type TestEngineTest struct {
	ID       string
	Name     string
	Scope    string
	Location string
	FileName string `json:"file_name,omitempty"`
	Result   string
	Tags     map[string]string `json:"tags,omitempty"`
}

// ReadTestEngineResults reads the Test Engine JSON results at path.
func ReadTestEngineResults(path string) ([]TestEngineTest, error) {
	var results []TestEngineTest
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read json: %v", err)
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("failed to parse json: %v", err)
	}
	return results, nil
}
