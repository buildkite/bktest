package runnerplugin

import "testing"

func TestReadTestEngineResults(t *testing.T) {
	results, err := ReadTestEngineResults("testdata/test-engine-result.json")
	if err != nil {
		t.Errorf("ReadTestEngineResults() error = %v", err)
	}
	if len(results) != 2 {
		t.Errorf("len(results) = %d, want 2", len(results))
	}
}
