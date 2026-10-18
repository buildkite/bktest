package bktest

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
)

var (
	executionsMu sync.Mutex
	executions   = map[testing.TB]*collector.Execution{}
	// pending holds executions whose Start cleanup has run but whose result
	// is not final yet, keyed by the name of the test's parent ("" for a
	// top-level test).
	pending = map[string][]pendingExecution{}
)

// pendingExecution is a test that has run its body and the cleanup Start
// registered, with its end time captured there. Cleanups run last-in
// first-out, so cleanups the test registered before calling Start are
// still to come and may fail it; the result is read once go test has
// finished the test for good.
type pendingExecution struct {
	ex   *collector.Execution
	t    testing.TB
	name string
	end  time.Time
}

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
		if inPanic() {
			// go test exits as soon as the panic resurfaces and Main never
			// regains control, so this is the last chance to deliver every
			// result and to report the export.
			finishAllPending()
			ex.Finish(end, result(t, true))
			c.Abort()
			fmt.Fprintln(os.Stderr, collector.ExportedLine(c.Exported(), id.Scope))
			return
		}
		park(pendingExecution{ex: ex, t: t, name: t.Name(), end: end})
		// A test's cleanups run only after its subtests have finished
		// theirs, so the subtests' results are final here.
		finishPending(t.Name())
	})
	return ex.Context()
}

// park holds an execution until its test's result is final. The parent is
// taken from the name: go test names a subtest <parent>/<name>.
func park(p pendingExecution) {
	parent := ""
	if i := strings.LastIndexByte(p.name, '/'); i >= 0 {
		parent = p.name[:i]
	}
	executionsMu.Lock()
	defer executionsMu.Unlock()
	pending[parent] = append(pending[parent], p)
}

// finishPending finishes the pending subtests of the named test, and
// theirs, from the results go test has settled on.
func finishPending(parent string) {
	executionsMu.Lock()
	subtests := pending[parent]
	delete(pending, parent)
	executionsMu.Unlock()
	for _, p := range subtests {
		finishPending(p.name)
		p.ex.Finish(p.end, result(p.t, false))
	}
}

// finishAllPending finishes every pending execution; Main calls it once
// m.Run has returned and every test is final.
func finishAllPending() {
	executionsMu.Lock()
	parents := slices.Collect(maps.Keys(pending))
	executionsMu.Unlock()
	for _, parent := range parents {
		finishPending(parent)
	}
}

// result maps the outcome of t onto a Result. go test runs Cleanup before
// it marks a panicking test as failed, so t.Failed is not enough on its
// own.
func result(t testing.TB, panicked bool) collector.Result {
	switch {
	case panicked:
		return collector.Result{
			Status: collector.StatusFail,
			Reason: "test panicked",
			Exception: &collector.Exception{
				Type:       "panic",
				Message:    "test panicked; go test prints the panic value after the test output",
				Stacktrace: string(debug.Stack()),
			},
		}
	case t.Failed():
		return collector.Result{Status: collector.StatusFail, Reason: "test failed"}
	case t.Skipped():
		return collector.Result{Status: collector.StatusSkipped}
	}
	return collector.Result{Status: collector.StatusPass}
}

type thisPackage struct{}

var bktestPackagePath = reflect.TypeOf(thisPackage{}).PkgPath()

// testPackage is the import path of the package Main ran in. Main sets it
// before publishing the collector, which orders it before any Start.
var testPackage string

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
// frame's file, which is the file the closure's enclosing function is
// compiled from. A subtest created by a helper in another package has no
// such frame on its stack, so its scope is the package Main ran in and the
// declaration is looked for in the package's test files.
func identify(t testing.TB) collector.Identity {
	id := collector.Identity{Name: t.Name(), Scope: testPackage}
	top, _, _ := strings.Cut(id.Name, "/")
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var frame runtime.Frame
	found, exact := false, false
	for more := true; more && !found; {
		var f runtime.Frame
		f, more = frames.Next()
		if _, fn := splitFunction(f.Function); fn == top || strings.HasPrefix(fn, top+".") {
			frame, found, exact = f, true, fn == top
		} else if frame.Function == "" && f.Function != "" && !strings.HasPrefix(f.Function, bktestPackagePath+".") {
			frame = f // the first caller outside this package, for the location only
		}
	}
	if found {
		id.Scope = importPath(frame.Function)
	}

	if exact {
		id.File, id.Line = frame.File, frame.Line
		if f := runtime.FuncForPC(frame.Entry); f != nil {
			id.File, id.Line = f.FileLine(f.Entry())
		}
		id.File = moduleRelative(id.File)
		return id
	}
	var (
		pos declaration
		ok  bool
	)
	if found {
		pos, ok = declaredIn(frame.File)[top]
	}
	if !ok {
		// Either no frame of the test function is on the stack, or Go 1.23
		// named a closure inlined from another file after the test.
		pos, ok = declarations()[top]
	}
	switch {
	case ok:
		id.File, id.Line = pos.file, pos.line
	case frame.Function != "":
		id.File, id.Line = frame.File, frame.Line
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

// importPath is the import path of the package a runtime function name
// belongs to. The _test suffix of an external test package is dropped so
// both halves of a package share one scope, the %xx escapes the linker
// applies to the last path element ("gopkg.in/yaml%2ev3") are undone, and
// "main", which is how functions in a main package are named, is resolved
// through the build info of the test binary.
func importPath(function string) string {
	pkg, _ := splitFunction(function)
	pkg = strings.TrimSuffix(pkg, "_test")
	if unescaped, err := url.PathUnescape(pkg); err == nil {
		pkg = unescaped
	}
	if pkg == "main" {
		if path := mainPackagePath(); path != "" {
			return path
		}
	}
	return pkg
}

// mainPackagePath is the import path of the package the test binary was
// built for, or "" when the build info does not record it: Go 1.24 records
// "<import path>.test" for every test binary, earlier releases only the
// import path of a main package.
var mainPackagePath = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return strings.TrimSuffix(info.Path, ".test")
})

type declaration struct {
	file string
	line int
}

var (
	declaredMu sync.Mutex
	declared   = map[string]func() map[string]declaration{}
)

// declaredIn maps every top-level function declared in the named source
// file to its position, parsing each file once, outside the lock so
// parallel tests do not queue behind a parse. A file name recorded by
// -trimpath is not a path on disk; go test runs the binary in the package's
// source directory, so the file is then looked for there.
func declaredIn(file string) map[string]declaration {
	declaredMu.Lock()
	parse, ok := declared[file]
	if !ok {
		parse = sync.OnceValue(func() map[string]declaration {
			decls := parseDeclarations(file)
			if decls == nil && !filepath.IsAbs(file) {
				if cwd, err := os.Getwd(); err == nil {
					decls = parseDeclarations(filepath.Join(cwd, filepath.Base(file)))
				}
			}
			return decls
		})
		declared[file] = parse
	}
	declaredMu.Unlock()
	return parse()
}

// parseDeclarations returns the top-level functions in the file, or nil
// when it cannot be read or parsed.
func parseDeclarations(path string) map[string]declaration {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	decls := map[string]declaration{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			decls[fn.Name.Name] = declaration{file: path, line: fset.Position(fn.Pos()).Line}
		}
	}
	return decls
}

// buildContext is the host's default build context with the -tags the test
// binary was built with, which Go 1.24 and later record in its build info.
// An earlier toolchain records nothing, so the default constraints stand.
var buildContext = sync.OnceValue(func() *build.Context {
	ctx := build.Default
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-tags" {
				ctx.BuildTags = strings.Split(setting.Value, ",")
			}
		}
	}
	return &ctx
})

// declarations maps every top-level function declared in the test files of
// the working directory, which go test makes the package's source
// directory, to its position. A function declared in several files, such
// as per-platform ones, is taken from the first file the build constraints
// select, and otherwise from whichever file declares it; a binary run
// elsewhere finds none.
var declarations = sync.OnceValue(func() map[string]declaration {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return nil
	}
	decls := map[string]declaration{}
	matched := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		selected, _ := buildContext().MatchFile(cwd, name)
		for fn, pos := range parseDeclarations(filepath.Join(cwd, name)) {
			if _, seen := decls[fn]; !seen || (selected && !matched[fn]) {
				decls[fn], matched[fn] = pos, selected
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
