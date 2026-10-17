package bktest

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const warningPrefix = "[buildkite-test-collector] "

var warnings = struct {
	mu   sync.Mutex
	out  io.Writer
	once map[string]bool
}{out: os.Stderr, once: map[string]bool{}}

func warnf(format string, args ...any) {
	warnings.mu.Lock()
	defer warnings.mu.Unlock()
	fmt.Fprintf(warnings.out, warningPrefix+format+"\n", args...)
}

// warnOnce prints the message the first time it is called with key and stays
// silent afterwards, so a per-test or per-span problem does not flood the
// output.
func warnOnce(key string, format string, args ...any) {
	warnings.mu.Lock()
	defer warnings.mu.Unlock()
	if warnings.once[key] {
		return
	}
	warnings.once[key] = true
	fmt.Fprintf(warnings.out, warningPrefix+format+"\n", args...)
}
