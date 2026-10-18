// Package many has one test with ten thousand subtests, whose results all
// become final at once when the parent finishes.
package many

import (
	"fmt"
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

const Subtests = 10000

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m))
}

func TestMany(t *testing.T) {
	bktest.Start(t)
	for i := range Subtests {
		t.Run(fmt.Sprintf("case-%05d", i), func(t *testing.T) {
			bktest.Start(t)
		})
	}
}
