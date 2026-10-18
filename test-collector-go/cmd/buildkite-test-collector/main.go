// Command buildkite-test-collector reads go test -json output on stdin,
// prints the tests' output as go test -v would, and exports every test
// execution to Buildkite Test Engine:
//
//	go test -json ./... | buildkite-test-collector
//
// It exits 1 when any test or package failed and 0 otherwise, whatever
// happens to the export; it exits 2 when stdin holds no go test -json
// events or the flags are wrong.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
	"github.com/buildkite/bktest/test-collector-go/internal/location"
	"github.com/buildkite/bktest/test-collector-go/internal/testjson"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]))
}

const (
	exitPass  = 0
	exitFail  = 1
	exitUsage = 2
)

func run(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	tags := tagFlags{}
	fs := flag.NewFlagSet("buildkite-test-collector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: go test -json ./... | buildkite-test-collector [-tag key=value]...")
		fs.PrintDefaults()
	}
	fs.Var(tags, "tag", "run-level tag, repeatable (key=value)")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return exitPass
	case err != nil:
		return exitUsage
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "buildkite-test-collector: unexpected argument %q; results are read from stdin\n", fs.Arg(0))
		fs.Usage()
		return exitUsage
	}

	defer collector.RedirectWarnings(stderr)()
	ctx := context.Background()
	c := setUp(ctx, collector.Options{Tags: tags, LocationPrefix: os.Getenv("BUILDKITE_ANALYTICS_LOCATION_PREFIX")})

	resolver := &location.Resolver{}
	if os.Getenv("BUILDKITE_ANALYTICS_DEBUG_ENABLED") != "" {
		resolver.Debugf = func(format string, args ...any) {
			fmt.Fprintf(stderr, "[buildkite-test-collector] debug: no test locations for "+format+"\n", args...)
		}
	}

	parser := testjson.NewParser(testjson.Handler{
		PackageStarted: func(pkg string) {
			if c != nil {
				resolver.Prefetch(ctx, pkg)
			}
		},
		Execution: func(e testjson.Execution) {
			if c == nil {
				return
			}
			pos := resolver.Lookup(ctx, e.Package, e.Test)
			if ex := c.Start(collector.Identity{Scope: e.Package, Name: e.Test, File: pos.File, Line: pos.Line}, e.Start); ex != nil {
				ex.Finish(e.End, e.Result)
			}
		},
		PackageFailedBeforeTests: func(pkg string) {
			collector.Warnf("package %s failed before running any tests (build failure or TestMain exit); no results were exported for it", pkg)
		},
		LibraryExported: func(pkg string) {
			collector.Warnf("package %s was also exported by the bktest library; results are duplicated — use one mode: pipe through buildkite-test-collector or call bktest.Main, not both", pkg)
		},
	})

	in := bufio.NewReader(stdin)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			output, perr := parser.Line(line)
			if errors.Is(perr, testjson.ErrNotEvent) {
				// Anything else on the stream, such as a go tool message,
				// passes through untouched.
				output = string(line)
			}
			io.WriteString(stdout, output)
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(stderr, "buildkite-test-collector: reading stdin: %v\n", err)
			}
			break
		}
	}
	parser.Finish()

	if c != nil {
		attempt("Could not shut down OpenTelemetry span export", c.Shutdown)
	}
	if parser.Events() == 0 {
		fmt.Fprintln(stderr, "buildkite-test-collector: no go test -json events on stdin; run go test with -json: go test -json ./... | buildkite-test-collector")
		return exitUsage
	}
	if parser.Failed() {
		return exitFail
	}
	return exitPass
}

// setUp configures export, turning any failure into a warning and a nil
// collector so a misconfigured collector never stops the output or changes
// the exit code.
func setUp(ctx context.Context, o collector.Options) (c *collector.Collector) {
	defer func() {
		if r := recover(); r != nil {
			collector.Warnf("OpenTelemetry span export disabled: %v\n%s", r, debug.Stack())
			c = nil
		}
	}()
	c, err := collector.New(ctx, o)
	if err != nil {
		collector.Warnf("OpenTelemetry span export disabled: %v", err)
		return nil
	}
	return c
}

func attempt(failure string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			collector.Warnf("%s: %v\n%s", failure, r, debug.Stack())
		}
	}()
	fn()
}

// tagFlags collects repeated -tag key=value flags.
type tagFlags map[string]string

func (t tagFlags) String() string {
	parts := make([]string, 0, len(t))
	for k, v := range t {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (t tagFlags) Set(s string) error {
	key, value, ok := strings.Cut(s, "=")
	if !ok || key == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	t[key] = value
	return nil
}
