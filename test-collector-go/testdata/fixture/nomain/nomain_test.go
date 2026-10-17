// Package nomain calls Start without a TestMain that runs bktest.Main.
package nomain

import (
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestOne(t *testing.T) {
	bktest.Start(t)
}

func TestTwo(t *testing.T) {
	bktest.Start(t)
}
