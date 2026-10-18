// Package location finds the file and line of top-level test functions by
// listing each package with the go tool and parsing its test files.
package location

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// listTimeout bounds each go list run so a stuck module download cannot
// hold up the result stream for long; the package then has no locations.
const listTimeout = 30 * time.Second

// Position is a test function's location relative to the module root, with
// forward slashes, as the collector reports it.
type Position struct {
	File string
	Line int
}

// Resolver looks up test locations one package at a time and remembers
// them. The zero value is ready to use.
type Resolver struct {
	// Dir is the directory to run go list in; empty means the current one.
	Dir string
	// Tags is the -tags value the tests were built with, so go list selects
	// the same files; GOFLAGS in the environment applies as it did to go
	// test.
	Tags string
	// Debugf, when set, receives one line per package that could not be
	// resolved.
	Debugf func(format string, args ...any)

	mu       sync.Mutex
	packages map[string]*packageLocations
	// listSlots bounds how many go list processes run at once, since
	// Prefetch is called for every package the stream mentions.
	listSlots chan struct{}
}

type packageLocations struct {
	once  sync.Once
	funcs map[string]Position
}

// Prefetch starts resolving pkg in the background so a later Lookup does
// not have to wait for go list.
func (r *Resolver) Prefetch(ctx context.Context, pkg string) {
	p := r.locations(pkg)
	go p.once.Do(func() { p.funcs = r.resolve(ctx, pkg) })
}

// Lookup returns the location of the top-level test function that test
// belongs to, or the zero Position when the package cannot be resolved.
// Subtests share their parent's location.
func (r *Resolver) Lookup(ctx context.Context, pkg, test string) Position {
	p := r.locations(pkg)
	p.once.Do(func() { p.funcs = r.resolve(ctx, pkg) })
	top, _, _ := strings.Cut(test, "/")
	return p.funcs[top]
}

func (r *Resolver) locations(pkg string) *packageLocations {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.packages == nil {
		r.packages = map[string]*packageLocations{}
		r.listSlots = make(chan struct{}, runtime.GOMAXPROCS(0))
	}
	p := r.packages[pkg]
	if p == nil {
		p = &packageLocations{}
		r.packages[pkg] = p
	}
	return p
}

type listedPackage struct {
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
	Module       *struct{ Dir string }
	Error        *struct{ Err string }
}

func (r *Resolver) resolve(ctx context.Context, pkg string) map[string]Position {
	select {
	case r.listSlots <- struct{}{}:
		defer func() { <-r.listSlots }()
	case <-ctx.Done():
		r.debugf("%s: %v before go list ran", pkg, ctx.Err())
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	args := []string{"list", "-json", "-e"}
	if r.Tags != "" {
		args = append(args, "-tags", r.Tags)
	}
	cmd := exec.CommandContext(ctx, "go", append(args, pkg)...)
	cmd.Dir = r.Dir
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			r.debugf("%s: go list failed: %v: %s", pkg, err, strings.TrimSpace(string(exit.Stderr)))
		} else {
			r.debugf("%s: go list failed: %v", pkg, err)
		}
		return nil
	}
	var lp listedPackage
	if err := json.Unmarshal(out, &lp); err != nil {
		r.debugf("%s: go list output: %v", pkg, err)
		return nil
	}
	if lp.Error != nil {
		r.debugf("%s: go list: %s", pkg, lp.Error.Err)
		return nil
	}
	root := lp.Dir
	if lp.Module != nil && lp.Module.Dir != "" {
		root = lp.Module.Dir
	}

	funcs := map[string]Position{}
	fset := token.NewFileSet()
	for _, name := range append(lp.TestGoFiles, lp.XTestGoFiles...) {
		path := filepath.Join(lp.Dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			r.debugf("%s: %v", pkg, err)
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, decl := range f.Decls {
			// Tests, examples and fuzz targets are all top-level
			// functions; the event's name picks the right one.
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = Position{File: filepath.ToSlash(rel), Line: fset.Position(fn.Pos()).Line}
			}
		}
	}
	return funcs
}

func (r *Resolver) debugf(format string, args ...any) {
	if r.Debugf != nil {
		r.Debugf(format, args...)
	}
}
