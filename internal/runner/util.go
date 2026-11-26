package runner

import "github.com/buildkite/bktest/v3/internal/plan"

func testCasesFromPaths(paths []string) []plan.TestCase {
	testCases := make([]plan.TestCase, len(paths))
	for i, path := range paths {
		testCases[i] = plan.TestCase{Path: path}
	}
	return testCases
}
