//go:build integration

// Package tagged only builds with -tags=integration, so its test file is
// excluded by the default build constraints the collector would otherwise
// scan with.
package tagged

import (
	"context"
	"os"
	"testing"

	"example.com/fixture/internal/testhelper"
	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m))
}

func TestTagged(t *testing.T) {
	bktest.Start(t)
	t.Run("sub", func(t *testing.T) {
		bktest.Start(t)
	})
	testhelper.RunCase(t, "helper", func(ctx context.Context, t *testing.T) {})
}
