package bktest

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
)

var (
	executionsMu sync.Mutex
	executions   = map[testing.TB]*collector.Execution{}
)

func lookup(t testing.TB) *collector.Execution {
	executionsMu.Lock()
	defer executionsMu.Unlock()
	return executions[t]
}

// startTest starts the execution for t and schedules its finish in
// t.Cleanup. The executions lock is held throughout so a test is never
// started twice and is published only once fully built.
func startTest(c *collector.Collector, t testing.TB) context.Context {
	executionsMu.Lock()
	defer executionsMu.Unlock()
	if ex, ok := executions[t]; ok {
		return ex.Context()
	}

	ex := c.Start(identify(t), time.Now())
	if ex == nil {
		return context.Background()
	}
	executions[t] = ex

	t.Cleanup(func() {
		end := time.Now()
		executionsMu.Lock()
		delete(executions, t)
		executionsMu.Unlock()
		finish(c, ex, t, end, inPanic())
	})
	return ex.Context()
}

// finish maps the outcome of t onto the execution. go test runs Cleanup
// before it marks a panicking test as failed, so t.Failed is not enough on
// its own.
func finish(c *collector.Collector, ex *collector.Execution, t testing.TB, end time.Time, panicked bool) {
	r := collector.Result{Status: collector.StatusPass}
	switch {
	case panicked:
		r = collector.Result{
			Status: collector.StatusFail,
			Reason: "test panicked",
			Exception: &collector.Exception{
				Type:       "panic",
				Message:    "test panicked; go test prints the panic value after the test output",
				Stacktrace: string(debug.Stack()),
			},
		}
	case t.Failed():
		r = collector.Result{Status: collector.StatusFail, Reason: "test failed"}
	case t.Skipped():
		r = collector.Result{Status: collector.StatusSkipped}
	}
	ex.Finish(end, r)

	if panicked {
		// go test exits as soon as the panic resurfaces; Main never regains
		// control, so this is the last chance to deliver the result.
		c.Abort()
	}
}

type identity = collector.Identity

type thisPackage struct{}

var bktestPackagePath = reflect.TypeOf(thisPackage{}).PkgPath()

// identify finds the first caller outside this package and derives the
// test's identity from that function: scope is the package import path,
// name is t.Name(), and the location is the file and first line of the
// function that called Start, relative to the module root.
func identify(t testing.TB) identity {
	id := identity{Name: t.Name()}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function != "" && !strings.HasPrefix(frame.Function, bktestPackagePath+".") {
			id.Scope = packagePath(frame.Function)
			id.File, id.Line = frame.File, frame.Line
			if fn := runtime.FuncForPC(frame.Entry); fn != nil {
				id.File, id.Line = fn.FileLine(fn.Entry())
			}
			id.File = moduleRelative(id.File)
			break
		}
		if !more {
			break
		}
	}
	if id.Scope == "" {
		id.Scope = "unknown"
	}
	return id
}

// packagePath extracts the import path from a runtime function name such
// as "example.com/m/pkg_test.TestX.func1", dropping the _test suffix of an
// external test package so both halves of a package share one scope. The
// linker escapes dots and other characters in the last path element as %xx
// ("gopkg.in/yaml%2ev3"), which is undone here.
func packagePath(function string) string {
	pkg := function
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		if j := strings.Index(pkg[i:], "."); j >= 0 {
			pkg = pkg[:i+j]
		}
	} else if j := strings.Index(pkg, "."); j >= 0 {
		pkg = pkg[:j]
	}
	pkg = strings.TrimSuffix(pkg, "_test")
	if unescaped, err := url.PathUnescape(pkg); err == nil {
		pkg = unescaped
	}
	return pkg
}

// moduleRelative strips the module root from a source path, so the location
// is stable across machines: the directory containing go.mod for absolute
// paths, the module path for the import-path-shaped names -trimpath records.
func moduleRelative(file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.ToSlash(file)
		if module := mainModulePath(); module != "" {
			return strings.TrimPrefix(file, module+"/")
		}
		return file
	}
	root := findModuleRoot(filepath.Dir(file))
	if root == "" {
		return filepath.ToSlash(file)
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return filepath.ToSlash(file)
	}
	return filepath.ToSlash(rel)
}

// mainModulePath is the module under test: from the build info when the
// toolchain fills it in for test binaries (Go 1.24+), otherwise from the
// go.mod above the working directory, which go test sets to the package's
// source directory.
var mainModulePath = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path != "" {
		return info.Main.Path
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root := findModuleRoot(cwd)
	if root == "" {
		return ""
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(gomod), "\n") {
		if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(module), `"`)
		}
	}
	return ""
})

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// inPanic reports whether the caller runs while a panic unwinds the
// goroutine, which is how go test runs Cleanup functions for a test that
// panicked.
func inPanic() bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function == "runtime.gopanic" {
			return true
		}
		if !more {
			return false
		}
	}
}
