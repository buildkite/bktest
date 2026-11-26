package pytest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
	"github.com/google/go-cmp/cmp"
)

func TestPytestRunParseJSON_CollectionError(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	json := `[
		{
			"id": "collection-error-id",
			"scope": "",
			"name": "tests/test_broken.py",
			"file_name": "tests/test_broken.py",
			"location": "tests/test_broken.py",
			"result": "failed",
			"tags": {"test.pytest_collection_error": "true"}
		}
	]`
	if err := os.WriteFile(resultPath, []byte(json), 0600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", resultPath, err)
	}

	var response runnerplugin.InterpretAttemptResponse
	if err := runParseJSON(&response, resultPath); err != nil {
		t.Fatalf("runParseJSON() error = %v", err)
	}

	if got, want := response.RunError, "pytest collection failed: tests/test_broken.py"; got != want {
		t.Errorf("runParseJSON() RunError = %q, want %q", got, want)
	}

	want := []runnerplugin.TestResult{{
		TestCase: runnerplugin.TestCase{
			Identifier: "collection-error-id",
			Format:     runnerplugin.FormatExample,
			Scope:      "",
			Name:       "tests/test_broken.py",
			Path:       "tests/test_broken.py",
		},
		Status: runnerplugin.StatusFailed,
	}}
	if diff := cmp.Diff(response.Tests, want); diff != "" {
		t.Errorf("runParseJSON() tests diff (-got +want):\n%s", diff)
	}
}

func TestPytestRunParseJSON_EmptyScopeWithoutCollectionErrorTag(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	json := `[
		{
			"id": "empty-scope-id",
			"scope": "",
			"name": "tests/test_file.py",
			"result": "failed"
		}
	]`
	if err := os.WriteFile(resultPath, []byte(json), 0600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", resultPath, err)
	}

	var response runnerplugin.InterpretAttemptResponse
	if err := runParseJSON(&response, resultPath); err != nil {
		t.Fatalf("runParseJSON() error = %v", err)
	}

	if response.RunError != "" {
		t.Errorf("runParseJSON() RunError = %q, want none", response.RunError)
	}

	want := []runnerplugin.TestResult{{
		TestCase: runnerplugin.TestCase{
			Identifier: "empty-scope-id",
			Format:     runnerplugin.FormatExample,
			Scope:      "",
			Name:       "tests/test_file.py",
			Path:       "tests/test_file.py",
		},
		Status: runnerplugin.StatusFailed,
	}}
	if diff := cmp.Diff(response.Tests, want); diff != "" {
		t.Errorf("runParseJSON() tests diff (-got +want):\n%s", diff)
	}
}

func TestPytestCommandNameAndArgs_WithSpacesInTestCase(t *testing.T) {
	testCases := []runnerplugin.TestCase{
		{Path: "foo/bar.py::TestFoo::test_foo[min-WeightedScalar-valid_reduce_ops0-only sum or avg are supported-2]"},
		{Path: "test_sample.py::test_simple"},
	}
	testCommand := "pytest {{testExamples}} --json={{resultPath}}"

	response, err := handler{}.PrepareAttempt(runnerplugin.PrepareAttemptRequest{
		Config:    runnerplugin.Config{TestCommand: testCommand, ResultPath: "result.json"},
		TestCases: testCases,
	})
	if err != nil {
		t.Fatalf("PrepareAttempt(%v, %q) error = %v", testCases, testCommand, err)
	}
	gotName, gotArgs := response.Argv[0], response.Argv[1:]

	wantName := "pytest"
	wantArgs := []string{
		"foo/bar.py::TestFoo::test_foo[min-WeightedScalar-valid_reduce_ops0-only sum or avg are supported-2]",
		"test_sample.py::test_simple",
		"--json=result.json",
	}

	if diff := cmp.Diff(gotName, wantName); diff != "" {
		t.Errorf("PrepareAttempt(%v, %q) name diff (-got +want):\n%s", testCases, testCommand, diff)
	}
	if diff := cmp.Diff(gotArgs, wantArgs); diff != "" {
		t.Errorf("PrepareAttempt(%v, %q) args diff (-got +want):\n%s", testCases, testCommand, diff)
	}
}

func TestPytestPathFromTestEngineResult(t *testing.T) {
	tests := []struct {
		name     string
		scope    string
		testName string
		wantPath string
	}{
		{
			name:     "scoped test",
			scope:    "tests/test_sample.py",
			testName: "test_happy",
			wantPath: "tests/test_sample.py::test_happy",
		},
		{
			name:     "collection error",
			scope:    "",
			testName: "tests/test_broken.py",
			wantPath: "tests/test_broken.py",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pytestPathFromTestEngineResult(tt.scope, tt.testName)
			if got != tt.wantPath {
				t.Errorf("pytestPathFromTestEngineResult(%q, %q) = %q, want %q", tt.scope, tt.testName, got, tt.wantPath)
			}
		})
	}
}

func TestPytestNodeIDFromJUnit(t *testing.T) {
	tests := []struct {
		classname string
		name      string
		wantScope string
		wantPath  string
	}{
		{
			classname: "test_sample",
			name:      "test_happy",
			wantPath:  "test_sample.py::test_happy",
		},
		{
			classname: "tests.test_sample",
			name:      "test_happy",
			wantPath:  "tests/test_sample.py::test_happy",
		},
		{
			classname: "tests.failed_test",
			name:      "test_failed",
			wantPath:  "tests/failed_test.py::test_failed",
		},
		{
			classname: "test_auth.TestLogin",
			name:      "test_success",
			wantPath:  "test_auth.py::TestLogin::test_success",
		},
		{
			classname: "tests.test_auth.TestLogin",
			name:      "test_success",
			wantPath:  "tests/test_auth.py::TestLogin::test_success",
		},
		{
			classname: "",
			name:      "test_something",
			wantPath:  "test_something",
		},
		{
			// Uppercase package directory: MyTest is a directory, TestSubtract is a class.
			classname: "tests.MyTest.test_subtract.TestSubtract",
			name:      "test_positive",
			wantScope: "tests/MyTest/test_subtract.py::TestSubtract",
			wantPath:  "tests/MyTest/test_subtract.py::TestSubtract::test_positive",
		},
		{
			// Uppercase package directory with no class: MyTest is a directory.
			classname: "tests.MyTest.test_add",
			name:      "test_add",
			wantPath:  "tests/MyTest/test_add.py::test_add",
		},
	}

	for _, tt := range tests {
		t.Run(tt.classname+"::"+tt.name, func(t *testing.T) {
			gotPath := pytestNodeIDFromJUnit(tt.classname, tt.name)
			if gotPath != tt.wantPath {
				t.Errorf("pytestNodeIDFromJUnit(%q, %q) path = %q, want %q", tt.classname, tt.name, gotPath, tt.wantPath)
			}
		})
	}
}

func TestParsePytestCollectOutput(t *testing.T) {
	output := `test_sample.py::test_happy
test_auth.py::TestLogin::test_success
test_auth.py::test_param[value1]

3 tests collected in 0.05s`

	got, err := parsePytestCollectOutput(output, false)
	if err != nil {
		t.Fatalf("parsePytestCollectOutput() error = %v", err)
	}

	want := []runnerplugin.TestCase{
		{Identifier: "test_sample.py::test_happy", Path: "test_sample.py::test_happy", Scope: "test_sample.py", Name: "test_happy", Format: runnerplugin.FormatExample},
		{Identifier: "test_auth.py::TestLogin::test_success", Path: "test_auth.py::TestLogin::test_success", Scope: "test_auth.py::TestLogin", Name: "test_success", Format: runnerplugin.FormatExample},
		{Identifier: "test_auth.py::test_param[value1]", Path: "test_auth.py::test_param[value1]", Scope: "test_auth.py", Name: "test_param[value1]", Format: runnerplugin.FormatExample},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("parsePytestCollectOutput() diff (-got +want):\n%s", diff)
	}
}

func TestParsePytestCollectOutput_JUnit(t *testing.T) {
	output := `test_sample.py::test_happy
test_auth.py::TestLogin::test_success
test_auth.py::test_param[value1]
tests/test_another.py::WithClass::test_method
tests/test_nested.py::TestOuter::TestInner::test_deep
tests/pkg.py/test_auth.py::TestLogin::test_success

6 tests collected in 0.05s`
	got, err := parsePytestCollectOutput(output, true)
	if err != nil {
		t.Fatalf("parsePytestCollectOutput() error = %v", err)
	}

	want := []runnerplugin.TestCase{
		{Identifier: "test_sample.py::test_happy", Path: "test_sample.py::test_happy", Scope: "test_sample", Name: "test_happy", Format: runnerplugin.FormatExample},
		{Identifier: "test_auth.py::TestLogin::test_success", Path: "test_auth.py::TestLogin::test_success", Scope: "test_auth.TestLogin", Name: "test_success", Format: runnerplugin.FormatExample},
		{Identifier: "test_auth.py::test_param[value1]", Path: "test_auth.py::test_param[value1]", Scope: "test_auth", Name: "test_param[value1]", Format: runnerplugin.FormatExample},
		{Identifier: "tests/test_another.py::WithClass::test_method", Path: "tests/test_another.py::WithClass::test_method", Scope: "tests.test_another.WithClass", Name: "test_method", Format: runnerplugin.FormatExample},
		// Nested classes produce more than one `::` in the scope.
		{Identifier: "tests/test_nested.py::TestOuter::TestInner::test_deep", Path: "tests/test_nested.py::TestOuter::TestInner::test_deep", Scope: "tests.test_nested.TestOuter.TestInner", Name: "test_deep", Format: runnerplugin.FormatExample},
		// A directory ending in `.py` must keep its extension; only the file loses it.
		{Identifier: "tests/pkg.py/test_auth.py::TestLogin::test_success", Path: "tests/pkg.py/test_auth.py::TestLogin::test_success", Scope: "tests.pkg.py.test_auth.TestLogin", Name: "test_success", Format: runnerplugin.FormatExample},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("parsePytestCollectOutput() diff (-got +want):\n%s", diff)
	}
}
