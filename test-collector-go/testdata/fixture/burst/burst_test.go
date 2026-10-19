// Package burst has one test with three subtests, whose results become
// final at once when the parent finishes.
package burst

import (
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m))
}

func TestBurst(t *testing.T) {
	bktest.Start(t)
	for _, name := range []string{"one", "two", "three"} {
		t.Run(name, func(t *testing.T) {
			bktest.Start(t)
		})
	}
}
