// Package fixture is a go test suite wired up the way a customer would wire
// one up. The collector's end-to-end test runs it in a subprocess and checks
// what reaches the loopback receiver.
package fixture

import (
	"context"
	"os"
	"testing"
	"time"

	bktest "github.com/buildkite/bktest/test-collector-go"
	"go.opentelemetry.io/otel"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m, bktest.WithTags(map[string]string{"team": "fixture"})))
}

func TestPass(t *testing.T) {
	bktest.Start(t)
	time.Sleep(20 * time.Millisecond)
}

func TestFail(t *testing.T) {
	bktest.Start(t)
	t.Error("deliberate failure")
}

func TestSkip(t *testing.T) {
	bktest.Start(t)
	t.Skip("deliberate skip")
}

func TestSubtests(t *testing.T) {
	bktest.Start(t)
	t.Run("a", func(t *testing.T) {
		bktest.Start(t)
	})
	t.Run("b", func(t *testing.T) {
		bktest.Start(t)
		t.Fail()
	})
}

func TestParallel(t *testing.T) {
	bktest.Start(t)
	for _, name := range []string{"p1", "p2"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bktest.Start(t)
			time.Sleep(10 * time.Millisecond)
		})
	}
}

func TestTagged(t *testing.T) {
	ctx := bktest.Start(t)
	bktest.Tag(t, "team", "overridden")
	bktest.Tag(t, "flavour", "vanilla")
	bktest.Annotate(t, "hello from the fixture")
	work(ctx)
}

func TestWithoutStart(t *testing.T) {
	// A test that never calls Start produces no execution.
}

func work(ctx context.Context) {
	_, span := otel.Tracer("fixture").Start(ctx, "fixture.work")
	defer span.End()
}
