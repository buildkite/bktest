package bktest

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	"go.opentelemetry.io/otel"
)

// TestMain silences the SDK's own error handler: the tests below provoke
// export failures on purpose and assert on the collector's warnings instead.
func TestMain(m *testing.M) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	os.Exit(m.Run())
}

func captureWarnings(t *testing.T) *otlptest.Buffer {
	t.Helper()
	buf := &otlptest.Buffer{}
	t.Cleanup(collector.RedirectWarnings(buf))
	return buf
}

// enabled makes this process look like one where Main configured export
// against r, for the duration of the test.
func enabled(t *testing.T, r *otlptest.Receiver, opts ...Option) *collector.Collector {
	t.Helper()
	otlptest.ClearEnv(t, map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "secret-token",
		"BUILDKITE_ANALYTICS_KEY":           "run-key-" + strings.ReplaceAll(t.Name(), "/", "_"),
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.Endpoint(),
	})
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	c := collector.Setup(context.Background(), newOptions(opts))
	if c == nil {
		t.Fatal("Setup returned no collector")
	}
	current.Store(c)
	mainCalled.Store(true)
	t.Cleanup(func() {
		current.Store(nil)
		mainCalled.Store(false)
		c.Shutdown()
	})
	return c
}

// fakeT stands in for a *testing.T so a test can drive an execution to any
// outcome without failing itself. Cleanups run when finish is called.
type fakeT struct {
	testing.TB
	name     string
	failed   bool
	skipped  bool
	cleanups []func()
}

func newFakeT(name string) *fakeT { return &fakeT{name: name} }

func (f *fakeT) Name() string      { return f.name }
func (f *fakeT) Failed() bool      { return f.failed }
func (f *fakeT) Skipped() bool     { return f.skipped }
func (f *fakeT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Helper()           {}
func (f *fakeT) Fail()             { f.failed = true }
func (f *fakeT) SkipNow()          { f.skipped = true }

// finish runs the registered cleanups in LIFO order, like testing does.
func (f *fakeT) finish() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	f.cleanups = nil
}

func TestResultMappingFromTestingTB(t *testing.T) {
	cases := []struct {
		name   string
		before func(ft *fakeT) // runs before Start
		drive  func(ft *fakeT) // runs after Start
		status string
		reason string
	}{
		{"passed", nil, nil, "pass", ""},
		{"failed", nil, func(ft *fakeT) { ft.Fail() }, "fail", "test failed"},
		{"skipped", nil, func(ft *fakeT) { ft.SkipNow() }, "skipped", ""},
		{"failed then skipped", nil, func(ft *fakeT) { ft.Fail(); ft.SkipNow() }, "fail", "test failed"},
		// A cleanup registered before Start runs after Start's own cleanup.
		{"failed by an earlier cleanup", func(ft *fakeT) { ft.Cleanup(ft.Fail) }, nil, "fail", "test failed"},
	}
	r := otlptest.NewReceiver(t)
	c := enabled(t, r)
	for _, tc := range cases {
		ft := newFakeT(tc.name)
		if tc.before != nil {
			tc.before(ft)
		}
		Start(ft)
		if tc.drive != nil {
			tc.drive(ft)
		}
		ft.finish()
	}
	finishAllPending()
	c.Shutdown()

	spans := r.TestSpans()
	if len(spans) != len(cases) {
		t.Fatalf("got %d spans, want %d: %v", len(spans), len(cases), otlptest.TestNames(spans))
	}
	for _, tc := range cases {
		s := otlptest.FindSpan(t, spans, tc.name)
		if got := otlptest.StringAttr(t, s.Attributes, "test.case.result.status"); got != tc.status {
			t.Errorf("%s status = %q, want %q", tc.name, got, tc.status)
		}
		if got := s.Status.GetMessage(); got != tc.reason {
			t.Errorf("%s reason = %q, want %q", tc.name, got, tc.reason)
		}
	}
}

func TestStartTwiceReturnsTheSameExecution(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := enabled(t, r)
	ft := newFakeT("TestTwice")
	ctx1 := Start(ft)
	ctx2 := Start(ft)
	if ctx1 != ctx2 {
		t.Fatal("second Start returned a different context")
	}
	ft.finish()
	finishAllPending()
	c.Shutdown()
	if n := len(r.TestSpans()); n != 1 {
		t.Fatalf("got %d test spans, want 1", n)
	}
}

func TestTagAndAnnotateTargetTheTest(t *testing.T) {
	r := otlptest.NewReceiver(t)
	c := enabled(t, r, WithTags(map[string]string{"team": "core"}))
	ft := newFakeT("TestTagged")
	Start(ft)
	Tag(ft, "team", "overridden")
	Tag(ft, "shard", "7")
	Annotate(ft, "note\xff")
	ft.finish()
	finishAllPending()
	c.Shutdown()

	s := r.TestSpans()[0]
	for k, v := range map[string]string{"buildkite.tag.team": "overridden", "buildkite.tag.shard": "7"} {
		if got := otlptest.StringAttr(t, s.Attributes, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(s.Events) != 1 || otlptest.StringAttr(t, s.Events[0].Attributes, "buildkite.annotation") != "note\uFFFD" {
		t.Errorf("events = %v", s.Events)
	}
}

func TestTagAndAnnotateBeforeStartWarnOnce(t *testing.T) {
	out := captureWarnings(t)
	ft := newFakeT("TestUnstarted")
	Tag(ft, "silent", "export is disabled, so nothing to warn about")
	Annotate(ft, "silent")
	if out.Len() != 0 {
		t.Fatalf("warned while disabled:\n%s", out.String())
	}

	enabled(t, otlptest.NewReceiver(t))
	Tag(ft, "k", "v")
	Tag(ft, "k2", "v")
	Annotate(ft, "x")
	Annotate(ft, "y")
	if n := strings.Count(out.String(), "bktest.Tag was called for TestUnstarted before bktest.Start"); n != 1 {
		t.Errorf("Tag warned %d times:\n%s", n, out.String())
	}
	if n := strings.Count(out.String(), "bktest.Annotate was called for TestUnstarted before bktest.Start"); n != 1 {
		t.Errorf("Annotate warned %d times:\n%s", n, out.String())
	}
}

func TestStartWithoutMainWarnsOnceAndReturnsAPlainContext(t *testing.T) {
	out := captureWarnings(t)
	current.Store(nil)
	mainCalled.Store(false)
	for range 2 {
		if ctx := Start(newFakeT("TestNoMain")); ctx != context.Background() {
			t.Fatalf("Start returned %v, want context.Background()", ctx)
		}
	}
	if n := strings.Count(out.String(), "bktest.Main did not run"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, out.String())
	}
}

func TestImportPath(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/widgets/calculator.TestAdd":                  "github.com/acme/widgets/calculator",
		"github.com/acme/widgets/calculator_test.TestAdd.func1":       "github.com/acme/widgets/calculator",
		"github.com/acme/widgets/v2.TestX":                            "github.com/acme/widgets/v2",
		"github.com/acme/go-collector/pkg.TestGeneric[...].func2":     "github.com/acme/go-collector/pkg",
		"github.com/acme/widgets/calculator.(*Suite).TestMethod":      "github.com/acme/widgets/calculator",
		"github.com/acme/dotted.name/pkg_test.TestDottedModule.func1": "github.com/acme/dotted.name/pkg",
		"gopkg.in/yaml%2ev3.TestUnmarshal":                            "gopkg.in/yaml.v3",
		"gopkg.in/yaml%2ev3_test.TestUnmarshal.func2":                 "gopkg.in/yaml.v3",
	}
	for in, want := range cases {
		if got := importPath(in); got != want {
			t.Errorf("importPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModuleRelativePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleRelative(filepath.Join(root, "pkg", "sub", "x_test.go")); got != "pkg/sub/x_test.go" {
		t.Errorf("moduleRelative = %q", got)
	}
	// -trimpath records the module path in place of the module root; this
	// test binary's module is the collector itself, at its root.
	if got := moduleRelative(bktestPackagePath + "/x_test.go"); got != "x_test.go" {
		t.Errorf("trimpath form = %q, want x_test.go", got)
	}
	if got := moduleRelative("example.com/other/pkg/x_test.go"); got != "example.com/other/pkg/x_test.go" {
		t.Errorf("a file from another module changed to %q", got)
	}
	if got := moduleRelative(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }
