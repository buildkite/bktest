//go:build integration

// This file only builds with -tags=integration; tagged_default_test.go
// declares a TestTagged of its own for every other build, the way
// per-platform test files do, so a scan under the wrong build constraints
// finds the wrong declaration.

package tagged

import (
	"context"
	"testing"

	"example.com/fixture/internal/testhelper"
	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestTagged(t *testing.T) {
	bktest.Start(t)
	t.Run("sub", func(t *testing.T) {
		bktest.Start(t)
	})
	testhelper.RunCase(t, "helper", func(ctx context.Context, t *testing.T) {})
}
