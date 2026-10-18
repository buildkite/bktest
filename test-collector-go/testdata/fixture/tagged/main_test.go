package tagged

import (
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m))
}
