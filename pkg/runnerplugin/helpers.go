package runnerplugin

import (
	"fmt"
	"io/fs"
	"os"

	"drjosh.dev/zzglob"
)

// DiscoverFiles returns the files matching pattern and not excludePattern,
// skipping node_modules, with the same glob semantics as bktest's built-in
// runners. It errors when no files match, as they do.
func DiscoverFiles(pattern, excludePattern string) ([]string, error) {
	parsedPattern, err := zzglob.Parse(pattern)
	if err != nil {
		return nil, fmt.Errorf("error parsing test file pattern %q", pattern)
	}
	parsedExcludePattern, err := zzglob.Parse(excludePattern)
	if err != nil {
		return nil, fmt.Errorf("error parsing test file exclude pattern %q", excludePattern)
	}

	files := []string{}
	err = parsedPattern.Glob(func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error walking at path %q: %v\n", path, err)
			return nil
		}
		if parsedExcludePattern.Match(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == "node_modules" {
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	}, zzglob.WalkIntermediateDirs(true))
	if err != nil {
		return nil, fmt.Errorf("error walking directory: %v", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found with pattern %q and exclude pattern %q", pattern, excludePattern)
	}
	return files, nil
}

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

// Logger writes debug logs to stderr when bktest runs with --debug.
type Logger struct{ Enabled bool }

func (l Logger) Printf(format string, args ...any) {
	if l.Enabled {
		fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
	}
}

// ExitCodeOr returns the command's exit code, or def if it didn't exit
// normally, such as when it couldn't start.
func (p InterpretParams) ExitCodeOr(def int) int {
	if p.ExitCode == nil {
		return def
	}
	return *p.ExitCode
}
