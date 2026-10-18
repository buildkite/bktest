package collector

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

// Warnf prints one collector warning line, to stderr unless RedirectWarnings
// is in effect.
func Warnf(format string, args ...any) {
	warnings.mu.Lock()
	defer warnings.mu.Unlock()
	fmt.Fprintf(warnings.out, warningPrefix+format+"\n", args...)
}

// WarnOnce prints the message the first time it is called with key and stays
// silent afterwards, so a per-test or per-span problem does not flood the
// output.
func WarnOnce(key string, format string, args ...any) {
	warnings.mu.Lock()
	defer warnings.mu.Unlock()
	if warnings.once[key] {
		return
	}
	warnings.once[key] = true
	fmt.Fprintf(warnings.out, warningPrefix+format+"\n", args...)
}

// RedirectWarnings sends warnings to w and forgets which once-only
// warnings have been printed, until the returned function is called. The
// command uses it to route warnings to its own stderr and tests use it to
// assert on them.
func RedirectWarnings(w io.Writer) (restore func()) {
	warnings.mu.Lock()
	defer warnings.mu.Unlock()
	prevOut, prevOnce := warnings.out, warnings.once
	warnings.out, warnings.once = w, map[string]bool{}
	return func() {
		warnings.mu.Lock()
		defer warnings.mu.Unlock()
		warnings.out, warnings.once = prevOut, prevOnce
	}
}
