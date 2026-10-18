package location

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":     "module example.com/mod\n\ngo 1.23\n",
		"pkg/pkg.go": "package pkg\n\nfunc Add(a, b int) int { return a + b }\n",
		"pkg/pkg_test.go": `package pkg

import "testing"

type suite struct{}

func (suite) TestMethod(t *testing.T) {}

func TestAdd(t *testing.T) {
	t.Run("sub", func(t *testing.T) {})
}

func helper() {}

func ExampleAdd() {
	// Output:
}

func FuzzAdd(f *testing.F) {}
`,
		"pkg/external_test.go": `package pkg_test

import "testing"

func TestExternal(t *testing.T) {}
`,
		"pkg/integration_test.go": `//go:build integration

package pkg

import "testing"

func TestIntegration(t *testing.T) {}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLookupFindsTopLevelTestFunctionsRelativeToTheModule(t *testing.T) {
	r := &Resolver{Dir: writeModule(t)}
	ctx := context.Background()

	if got := r.Lookup(ctx, "example.com/mod/pkg", "TestAdd"); got != (Position{File: "pkg/pkg_test.go", Line: 9}) {
		t.Errorf("TestAdd = %+v", got)
	}
	if got := r.Lookup(ctx, "example.com/mod/pkg", "TestAdd/sub"); got != (Position{File: "pkg/pkg_test.go", Line: 9}) {
		t.Errorf("subtest = %+v, want the parent's location", got)
	}
	if got := r.Lookup(ctx, "example.com/mod/pkg", "TestExternal"); got != (Position{File: "pkg/external_test.go", Line: 5}) {
		t.Errorf("external test package = %+v", got)
	}
	if got := r.Lookup(ctx, "example.com/mod/pkg", "ExampleAdd"); got != (Position{File: "pkg/pkg_test.go", Line: 15}) {
		t.Errorf("ExampleAdd = %+v", got)
	}
	if got := r.Lookup(ctx, "example.com/mod/pkg", "FuzzAdd/seed#0"); got != (Position{File: "pkg/pkg_test.go", Line: 19}) {
		t.Errorf("FuzzAdd/seed#0 = %+v", got)
	}
	if got := r.Lookup(ctx, "example.com/mod/pkg", "TestMethod"); got != (Position{}) {
		t.Errorf("method named Test* = %+v, want none", got)
	}
}

func TestBuildTagsSelectTheFilesGoTestCompiled(t *testing.T) {
	dir := writeModule(t)
	ctx := context.Background()
	want := Position{File: "pkg/integration_test.go", Line: 7}

	if got := (&Resolver{Dir: dir}).Lookup(ctx, "example.com/mod/pkg", "TestIntegration"); got != (Position{}) {
		t.Errorf("without tags = %+v, want none: the file is not compiled", got)
	}
	if got := (&Resolver{Dir: dir, Tags: "integration"}).Lookup(ctx, "example.com/mod/pkg", "TestIntegration"); got != want {
		t.Errorf("with -tags = %+v, want %+v", got, want)
	}
	t.Setenv("GOFLAGS", "-tags=integration")
	if got := (&Resolver{Dir: dir}).Lookup(ctx, "example.com/mod/pkg", "TestIntegration"); got != want {
		t.Errorf("with GOFLAGS = %+v, want %+v", got, want)
	}
}

func TestUnresolvablePackageYieldsNoLocationAndOneDebugLine(t *testing.T) {
	var debug []string
	r := &Resolver{
		Dir:    writeModule(t),
		Debugf: func(format string, args ...any) { debug = append(debug, fmt.Sprintf(format, args...)) },
	}
	ctx := context.Background()
	r.Prefetch(ctx, "example.com/elsewhere/pkg")
	for _, test := range []string{"TestA", "TestB", "TestA/sub"} {
		if got := r.Lookup(ctx, "example.com/elsewhere/pkg", test); got != (Position{}) {
			t.Errorf("%s = %+v", test, got)
		}
	}
	if len(debug) != 1 || !strings.HasPrefix(debug[0], "example.com/elsewhere/pkg: ") {
		t.Errorf("debug = %q, want one line naming the package", debug)
	}
}
