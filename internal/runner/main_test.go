package runner

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the tests their own temporary directory. Each runner keeps
// a state directory until it is closed, and tests don't close the runners
// they create.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bktest-runner-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
