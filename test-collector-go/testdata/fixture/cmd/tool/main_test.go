package main

import (
	"os"
	"testing"

	bktest "github.com/buildkite/bktest/test-collector-go"
)

func TestMain(m *testing.M) {
	os.Exit(bktest.Main(m))
}

func TestAnswer(t *testing.T) {
	bktest.Start(t)
	if answer() != 42 {
		t.Fatal("wrong answer")
	}
}
