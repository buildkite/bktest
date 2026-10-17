// Package panicky holds a test that panics, which makes go test abort the
// whole binary. It lives in its own package so the other fixture packages
// finish normally.
package panicky

import (
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

// An explicit prefix, even an empty one, beats BUILDKITE_ANALYTICS_LOCATION_PREFIX.
func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m, bktest.WithLocationPrefix("")))
}

func TestBeforePanic(t *testing.T) {
	bktest.Start(t)
}

// TestPanics panics in a subtest: go test runs the subtest's cleanups and
// then the parent's before it dies, so both results must be delivered.
func TestPanics(t *testing.T) {
	bktest.Start(t)
	t.Run("child", func(t *testing.T) {
		bktest.Start(t)
		panic("boom")
	})
}
