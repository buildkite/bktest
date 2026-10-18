package bktest

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
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

	id := identify(t)
	ex := c.Start(id, time.Now())
	if ex == nil {
		return context.Background()
	}
	executions[t] = ex

	t.Cleanup(func() {
		end := time.Now()
		executionsMu.Lock()
		delete(executions, t)
		executionsMu.Unlock()
		finish(c, ex, t, end, inPanic(), id.Scope)
	})
	return ex.Context()
}

// finish maps the outcome of t onto the execution. go test runs Cleanup
// before it marks a panicking test as failed, so t.Failed is not enough on
// its own.
func finish(c *collector.Collector, ex *collector.Execution, t testing.TB, end time.Time, panicked bool, importPath string) {
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
		// control, so this is the last chance to deliver the result and to
		// report the export.
		c.Abort()
		fmt.Fprintln(os.Stderr, collector.ExportedLine(c.Exported(), importPath))
	}
}

type thisPackage struct{}

var bktestPackagePath = reflect.TypeOf(thisPackage{}).PkgPath()

// testPackage is the import path of the package under test, recorded by
// Main, for tests whose own function is not on the stack when Start runs.
var testPackage atomic.Pointer[string]

// identify derives the test's identity the way the buildkite-test-collector
// command derives it from the go test -json stream, so a test keeps its
// identity when a suite switches modes: scope is the package import path,
// name is t.Name(), and the location is the declaration of the top-level
// test function, shared by its subtests.
//
// The top-level function is the one named t.Name() up to the first "/".
// Its frame is either that function or a closure inside it, which the
// runtime names TestX.func1, TestX.func1.1 and so on, and that frame gives
// the scope; helpers in other packages that call Start on a test's behalf
// therefore never become the scope. The location is read from the frame
// when it is the function itself, otherwise from the declaration in the
// package's test files, which go test runs the binary in. A subtest created
// by a helper in another package has no such frame on its stack, so its
// scope is the package Main ran in.
func identify(t testing.TB) collector.Identity {
	id := collector.Identity{Name: t.Name()}
	top, _, _ := strings.Cut(id.Name, "/")
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var chosen, fallback *runtime.Frame
	for chosen == nil {
		frame, more := frames.Next()
		if frame.Function != "" {
			if _, fn := splitFunction(frame.Function); fn == top || strings.HasPrefix(fn, top+".") {
				chosen = &frame
			} else if fallback == nil && !strings.HasPrefix(frame.Function, bktestPackagePath+".") {
				fallback = &frame
			}
		}
		if !more {
			break
		}
	}

	switch {
	case chosen != nil:
		id.Scope = importPath(chosen.Function)
	case testPackage.Load() != nil:
		id.Scope = *testPackage.Load()
	case fallback != nil:
		id.Scope = importPath(fallback.Function)
	default:
		id.Scope = "unknown"
	}

	exact := false
	if chosen != nil {
		_, fn := splitFunction(chosen.Function)
		exact = fn == top
	} else {
		chosen = fallback
	}
	if !exact {
		if pos, ok := declarations()[top]; ok {
			id.File, id.Line = moduleRelative(pos.file), pos.line
			return id
		}
	}
	if chosen == nil {
		return id
	}
	id.File, id.Line = chosen.File, chosen.Line
	if f := runtime.FuncForPC(chosen.Entry); f != nil {
		id.File, id.Line = f.FileLine(f.Entry())
	}
	id.File = moduleRelative(id.File)
	return id
}

// splitFunction splits a runtime function name such as
// "example.com/m/pkg_test.TestX.func1" into its package part
// ("example.com/m/pkg_test") and function part ("TestX.func1").
func splitFunction(function string) (pkg, fn string) {
	i := strings.LastIndex(function, "/") + 1
	j := strings.Index(function[i:], ".")
	if j < 0 {
		return function, ""
	}
	return function[:i+j], function[i+j+1:]
}

// packagePath extracts the package part of a runtime function name,
// dropping the _test suffix of an external test package so both halves of
// a package share one scope. The linker escapes dots and other characters
// in the last path element as %xx ("gopkg.in/yaml%2ev3"), which is undone
// here.
func packagePath(function string) string {
	pkg, _ := splitFunction(function)
	pkg = strings.TrimSuffix(pkg, "_test")
	if unescaped, err := url.PathUnescape(pkg); err == nil {
		pkg = unescaped
	}
	return pkg
}

// importPath is packagePath with the one case where the runtime name is not
// the import path resolved: functions in package main are named "main.F",
// and the test binary's build info records the import path of a main
// package under test.
func importPath(function string) string {
	pkg := packagePath(function)
	if pkg != "main" {
		return pkg
	}
	if path := mainPackagePath(); path != "" {
		return path
	}
	return pkg
}

var mainPackagePath = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	// Go 1.24 fills Path in for every test binary as "<import path>.test";
	// for a main package every supported release records the import path.
	return strings.TrimSuffix(info.Path, ".test")
})

type declaration struct {
	file string
	line int
}

// declarations maps every top-level function declared in the package's
// test files to its position. go test runs the binary in the package's
// source directory, so the files are read from the working directory; a
// binary run elsewhere finds none, and subtests then report the line of
// their closure.
var declarations = sync.OnceValue(func() map[string]declaration {
	decls := map[string]declaration{}
	cwd, err := os.Getwd()
	if err != nil {
		return decls
	}
	files, _ := filepath.Glob(filepath.Join(cwd, "*_test.go"))
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				if _, seen := decls[fn.Name.Name]; !seen {
					decls[fn.Name.Name] = declaration{file: path, line: fset.Position(fn.Pos()).Line}
				}
			}
		}
	}
	return decls
})

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
