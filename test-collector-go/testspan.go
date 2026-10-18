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

// identify derives the test's identity from the frame of the test function
// itself, so it matches what the buildkite-test-collector command reports
// from the go test -json stream: scope is the package import path, name is
// t.Name(), and the location is the declaration of the top-level test
// function, shared by its subtests.
//
// The top-level function is the one whose name is t.Name() up to the first
// "/"; its frame is either that function or a closure inside it (which the
// runtime names TestX.func1, TestX.func1.1, ...). Helpers in other packages
// that call Start on a test's behalf therefore do not become the scope.
// When no frame matches, which happens for a subtest closure created by a
// helper in another package, the identity resolved earlier for the same
// top-level test is reused, and failing that the first caller outside this
// package is used.
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

	topLevelMu.Lock()
	defer topLevelMu.Unlock()
	matched := chosen != nil
	if !matched {
		if known, ok := topLevel[top]; ok {
			id.Scope, id.File, id.Line = known.Scope, known.File, known.Line
			return id
		}
		chosen = fallback
	}
	if chosen == nil {
		id.Scope = "unknown"
		return id
	}

	id.Scope = importPath(chosen.Function)
	id.File, id.Line = chosen.File, chosen.Line
	if fn := runtime.FuncForPC(chosen.Entry); fn != nil {
		id.File, id.Line = fn.FileLine(fn.Entry())
	}
	if _, fn := splitFunction(chosen.Function); fn != top && strings.HasPrefix(fn, top+".") {
		// A subtest closure: report the enclosing test function instead.
		if line, ok := declarationLine(id.File, top); ok {
			id.Line = line
		}
	}
	id.File = moduleRelative(id.File)
	if matched {
		topLevel[top] = id
	}
	return id
}

// topLevel remembers, by top-level test name, the identity resolved from a
// matching frame. A test binary holds one package, so the name is unique.
var (
	topLevelMu sync.Mutex
	topLevel   = map[string]collector.Identity{}
)

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

// declarationLine is the line on which the top-level function name is
// declared in file. The file is parsed once; under -trimpath the recorded
// path is not on disk, so the file is also looked for in the working
// directory, which go test sets to the package's source directory.
func declarationLine(file, name string) (int, bool) {
	decls := declarationsOf(file)
	line, ok := decls[name]
	return line, ok
}

var (
	declarationsMu sync.Mutex
	declarations   = map[string]map[string]int{}
)

func declarationsOf(file string) map[string]int {
	declarationsMu.Lock()
	defer declarationsMu.Unlock()
	if decls, ok := declarations[file]; ok {
		return decls
	}
	decls := map[string]int{}
	fset := token.NewFileSet()
	for _, path := range []string{file, filepath.Base(file)} {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				decls[fn.Name.Name] = fset.Position(fn.Pos()).Line
			}
		}
		break
	}
	declarations[file] = decls
	return decls
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
