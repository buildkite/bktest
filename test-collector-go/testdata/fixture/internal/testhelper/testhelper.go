// Package testhelper wraps bktest.Start the way a suite's shared test
// utilities would, so the fixture can check that the helper's package never
// becomes a test's scope or location.
package testhelper

import (
	"context"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

// Start is bktest.Start called through another package.
func Start(t *testing.T) context.Context {
	t.Helper()
	return bktest.Start(t)
}

// RunCase runs name as a subtest that starts through this package.
func RunCase(t *testing.T, name string, fn func(ctx context.Context, t *testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		fn(bktest.Start(t), t)
	})
}
