package bktest

import (
	"bytes"
	"context"
	"errors"
	"go/version"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const fixtureModule = "example.com/fixture"

// goTest runs go test for the fixture package in a subprocess with a clean
// collector environment plus vars, and returns its combined output and exit
// code. The subprocess is the only place the collector meets the real
// testing package: TestMain, t.Cleanup ordering, panics and exit codes.
func goTest(t *testing.T, pkg string, vars map[string]string, flags ...string) (string, int) {
	t.Helper()
	return goTestCacheable(t, pkg, vars, append([]string{"-count=1"}, flags...)...)
}

// goTestCacheable is goTest without -count=1, so go test may serve the
// package from its cache.
func goTestCacheable(t *testing.T, pkg string, vars map[string]string, flags ...string) (string, int) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go test in a subprocess")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append(append([]string{"test", "-v"}, flags...), pkg)...)
	cmd.Dir = filepath.Join("testdata", "fixture")
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); !otlptest.CollectorEnvVar(k) {
			cmd.Env = append(cmd.Env, e)
		}
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit):
		return out.String(), exit.ExitCode()
	default:
		t.Fatalf("go test %s: %v\n%s", pkg, err, out.String())
		return "", -1
	}
}

func exportedTo(r *otlptest.Receiver, runKey string) map[string]string {
	return map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "fixture-token",
		"BUILDKITE_ANALYTICS_KEY":           runKey,
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.Endpoint(),
		// The SDK's own configuration must not reach the exporter.
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:9/",
		"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=garbage",
	}
}

// fixtureLine finds the line in the fixture file matching pattern,
// independently of the collector's own frame walking.
func fixtureLine(t *testing.T, file, pattern string) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "fixture", file))
	if err != nil {
		t.Fatal(err)
	}
	loc := regexp.MustCompile("(?m)" + pattern).FindIndex(src)
	if loc == nil {
		t.Fatalf("%s not found in %s", pattern, file)
	}
	return bytes.Count(src[:loc[0]], []byte("\n")) + 1
}

func TestEndToEndWithGoTest(t *testing.T) {
	r := otlptest.NewReceiver(t)
	vars := exportedTo(r, "fixture-run")
	vars["BUILDKITE_ANALYTICS_LOCATION_PREFIX"] = "svc/"
	out, code := goTest(t, ".", vars)
	if code != 1 {
		t.Fatalf("go test exit code = %d, want 1 (TestFail fails)\n%s", code, out)
	}
	for _, want := range []string{"--- FAIL: TestFail", "--- SKIP: TestSkip", "--- FAIL: TestSubtests/b"} {
		if !strings.Contains(out, want) {
			t.Errorf("go test output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[buildkite-test-collector]") {
		t.Errorf("a clean run must not warn:\n%s", out)
	}
	if want := collector.ExportedLine(16, fixtureModule); strings.Count(out, want) != 1 {
		t.Errorf("go test output lacks %q:\n%s", want, out)
	}

	// Locations are the declaration of the top-level test function, shared
	// by its subtests, as the buildkite-test-collector command reports them;
	// a Start called through a helper in another package (TestViaHelper and
	// its subtest, and the TestHelperSubtests subtests whose parent never
	// called Start) must not point at the helper.
	fixtureLines := map[string]int{}
	for _, name := range []string{"TestPass", "TestFail", "TestSkip", "TestSubtests", "TestParallel", "TestTagged", "TestViaHelper", "TestHelperSubtests", "TestCleanupBeforeStart"} {
		fixtureLines[name] = fixtureLine(t, "fixture_test.go", `^func `+name+`\(`)
	}
	for _, sub := range []string{"TestSubtests/a", "TestSubtests/b", "TestParallel/p1", "TestParallel/p2", "TestViaHelper/case", "TestHelperSubtests/case", "TestHelperSubtests/inline", "TestCleanupBeforeStart/sub"} {
		top, _, _ := strings.Cut(sub, "/")
		fixtureLines[sub] = fixtureLines[top]
	}

	spans := r.TestSpans()
	want := map[string]string{
		"TestPass":           "pass",
		"TestFail":           "fail",
		"TestSkip":           "skipped",
		"TestSubtests":       "fail",
		"TestSubtests/a":     "pass",
		"TestSubtests/b":     "fail",
		"TestParallel":       "pass",
		"TestParallel/p1":    "pass",
		"TestParallel/p2":    "pass",
		"TestTagged":         "pass",
		"TestViaHelper":      "pass",
		"TestViaHelper/case": "pass",
		// TestHelperSubtests never calls Start, so only its subtests are reported.
		"TestHelperSubtests/case":   "pass",
		"TestHelperSubtests/inline": "pass",
		// Failed by cleanups registered before Start, which run after
		// Start's own cleanup: the result go test reports, not the one
		// visible when Start's cleanup ran.
		"TestCleanupBeforeStart":     "fail",
		"TestCleanupBeforeStart/sub": "fail",
	}
	if len(spans) != len(want) {
		t.Fatalf("got %d test spans, want %d: %v", len(spans), len(want), otlptest.TestNames(spans))
	}
	for name, status := range want {
		s := otlptest.FindSpan(t, spans, name)
		if got := otlptest.StringAttr(t, s.Attributes, "test.case.result.status"); got != status {
			t.Errorf("%s status = %q, want %q", name, got, status)
		}
		if got := otlptest.StringAttr(t, s.Attributes, "buildkite.test.scope"); got != fixtureModule {
			t.Errorf("%s scope = %q, want %q", name, got, fixtureModule)
		}
		if got := otlptest.StringAttr(t, s.Attributes, "code.file.path"); got != "svc/fixture_test.go" {
			t.Errorf("%s file = %q, want svc/fixture_test.go (BUILDKITE_ANALYTICS_LOCATION_PREFIX applied)", name, got)
		}
		if got, wantLine := otlptest.IntAttr(t, s.Attributes, "code.line.number"), fixtureLines[name]; got != wantLine {
			t.Errorf("%s line = %d, want %d (the top-level test function's declaration)", name, got, wantLine)
		}
		if got := otlptest.StringAttr(t, s.Attributes, "test.case.name"); got != fixtureModule+"."+name {
			t.Errorf("%s test.case.name = %q", name, got)
		}
		if got := otlptest.StringAttr(t, s.Attributes, "buildkite.tag."+"team"); got != "fixture" && name != "TestTagged" {
			t.Errorf("%s team tag = %q, want fixture", name, got)
		}
		if (s.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (status == "fail") {
			t.Errorf("%s span status = %v for result %s", name, s.Status.GetCode(), status)
		}
	}

	pass := otlptest.FindSpan(t, spans, "TestPass")
	if d := time.Duration(pass.EndTimeUnixNano - pass.StartTimeUnixNano); d < 20*time.Millisecond || d > 10*time.Second {
		t.Errorf("TestPass duration = %s, want about 20ms", d)
	}

	tagged := otlptest.FindSpan(t, spans, "TestTagged")
	if got := otlptest.StringAttr(t, tagged.Attributes, "buildkite.tag."+"team"); got != "overridden" {
		t.Errorf("per-test tag did not override the run tag: %q", got)
	}
	if got := otlptest.StringAttr(t, tagged.Attributes, "buildkite.tag."+"flavour"); got != "vanilla" {
		t.Errorf("flavour tag = %q", got)
	}
	var annotated bool
	for _, ev := range tagged.Events {
		if ev.Name == "test.annotation" {
			annotated = otlptest.StringAttr(t, ev.Attributes, "buildkite.annotation") == "hello from the fixture"
		}
	}
	if !annotated {
		t.Errorf("TestTagged has no annotation event: %v", tagged.Events)
	}

	children := r.SpansOn(otlptest.StreamChild)
	byName := map[string][]*tracepb.Span{}
	for _, s := range children {
		byName[s.Name] = append(byName[s.Name], s)
	}
	if n := len(byName["fixture.work"]); n != 1 {
		t.Fatalf("fixture.work forwarded %d times, want 1: %v", n, r.ChildNames())
	}
	work := byName["fixture.work"][0]
	if hexOf(work.TraceId) != hexOf(tagged.TraceId) {
		t.Errorf("fixture.work is not in TestTagged's trace")
	}
	var bodyOfTagged *tracepb.Span
	for _, b := range byName["test.body"] {
		if hexOf(b.TraceId) == hexOf(tagged.TraceId) {
			bodyOfTagged = b
		}
	}
	if bodyOfTagged == nil || hexOf(work.ParentSpanId) != hexOf(bodyOfTagged.SpanId) {
		t.Errorf("fixture.work should be a child of TestTagged's body phase")
	}
	// Failed tests export their body phase even when empty; passing empty
	// tests do not.
	bodies := 0
	for _, b := range byName["test.body"] {
		bodies++
		if hexOf(b.TraceId) == hexOf(pass.TraceId) {
			t.Errorf("TestPass has an empty body phase that should not export")
		}
	}
	if bodies != 6 { // TestFail, TestSubtests, TestSubtests/b, TestTagged, TestCleanupBeforeStart and its subtest
		t.Errorf("got %d body phases, want 6: %v", bodies, r.ChildNames())
	}

	for _, req := range r.All() {
		if got := req.Header.Get(otlptest.RunKeyHeader); got != "fixture-run" {
			t.Errorf("run key header = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != `Token token="fixture-token"` {
			t.Errorf("authorization = %q", got)
		}
	}
}

// Results become final in bursts: a parent's subtests all at once when the
// parent finishes, and everything else when m.Run returns. None may be lost
// to the queue in between.
func TestEndToEndTenThousandSubtestsAllArrive(t *testing.T) {
	r := otlptest.NewReceiver(t)
	out, code := goTest(t, "./many", exportedTo(r, "fixture-many"))
	if code != 0 {
		t.Fatalf("exit code = %d:\n%s", code, out)
	}
	if strings.Contains(out, "TEST RESULTS MISSING") {
		t.Errorf("results were dropped:\n%s", out)
	}
	if n := len(r.TestSpans()); n != 10001 {
		t.Fatalf("got %d test spans, want 10001", n)
	}
	if want := collector.ExportedLine(10001, fixtureModule+"/many"); !strings.Contains(out, want) {
		t.Errorf("go test output lacks %q", want)
	}
}

// Waiting for the exporter, whether its queue is small or its endpoint is
// slow, never happens on a test goroutine, so it never counts against
// go test -timeout and never changes a result.
func TestEndToEndWaitingForTheExporterNeverTimesOutATest(t *testing.T) {
	t.Run("small queue", func(t *testing.T) {
		t.Parallel()
		r := otlptest.NewReceiver(t)
		vars := exportedTo(r, "fixture-burst")
		vars["BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE"] = "2"
		vars["BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE"] = "2"
		out, code := goTest(t, "./burst", vars, "-timeout=500ms")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0:\n%s", code, out)
		}
		if n := len(r.TestSpans()); n != 4 {
			t.Errorf("got %d test spans, want 4:\n%s", n, out)
		}
	})
	t.Run("stalled endpoint", func(t *testing.T) {
		t.Parallel()
		r := otlptest.NewReceiver(t)
		const stall = 6 * time.Second
		r.Delay.Store(int64(stall))
		time.AfterFunc(stall, func() { r.Delay.Store(0) })
		out, code := goTest(t, "./many", exportedTo(r, "fixture-many-stalled"), "-timeout=3s")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0:\n%s", code, out)
		}
		if strings.Contains(out, "TEST RESULTS MISSING") {
			t.Errorf("results were dropped:\n%s", out)
		}
		if n := len(r.TestSpans()); n != 10001 {
			t.Errorf("got %d test spans, want 10001", n)
		}
	})
}

// A test file behind a build tag is compiled all the same, so its
// subtests, including one created by a helper in another package, are
// located at the test function's declaration.
func TestEndToEndBuildTagsDoNotMoveLocations(t *testing.T) {
	r := otlptest.NewReceiver(t)
	out, code := goTest(t, "./tagged", exportedTo(r, "fixture-tagged"), "-tags=integration")
	if code != 0 {
		t.Fatalf("exit code = %d:\n%s", code, out)
	}
	spans := r.TestSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d test spans, want 3: %v", len(spans), otlptest.TestNames(spans))
	}
	line := fixtureLine(t, filepath.Join("tagged", "tagged_test.go"), `^func TestTagged\(`)
	for _, name := range []string{"TestTagged", "TestTagged/sub", "TestTagged/helper"} {
		if name == "TestTagged/helper" && !buildInfoRecordsTags() {
			// A subtest without a frame of its test function is looked
			// up in the package's test files, which only Go 1.24 and
			// later can read under the right build tags.
			continue
		}
		s := otlptest.FindSpan(t, spans, name)
		if got := otlptest.StringAttr(t, s.Attributes, "code.file.path"); got != "tagged/tagged_test.go" {
			t.Errorf("%s file = %q", name, got)
		}
		if got := otlptest.IntAttr(t, s.Attributes, "code.line.number"); got != line {
			t.Errorf("%s line = %d, want %d (the declaration of TestTagged)", name, got, line)
		}
	}
}

// buildInfoRecordsTags reports whether the toolchain running the fixtures
// records -tags in a test binary's build info, which Go 1.24 introduced.
func buildInfoRecordsTags() bool {
	return version.Compare(runtime.Version(), "go1.24") >= 0
}

// go test caches a package's result keyed on the environment variables the
// test binary read while running, so a run with a different run key or
// endpoint is not served from the cache and is reported again.
func TestEndToEndCachedPackageRerunsWhenTheRunChanges(t *testing.T) {
	first, second := otlptest.NewReceiver(t), otlptest.NewReceiver(t)
	cache := map[string]string{"GOCACHE": t.TempDir()}
	run := func(r *otlptest.Receiver, runKey string) string {
		vars := exportedTo(r, runKey)
		maps.Copy(vars, cache)
		out, code := goTestCacheable(t, "./cmd/tool", vars)
		if code != 0 {
			t.Fatalf("exit code = %d:\n%s", code, out)
		}
		return out
	}
	run(first, "fixture-cache-1")
	if out := run(second, "fixture-cache-2"); strings.Contains(out, "(cached)") {
		t.Fatalf("the second run was served from the cache:\n%s", out)
	}
	if n, m := len(first.TestSpans()), len(second.TestSpans()); n != 1 || m != 1 {
		t.Errorf("got %d and %d test spans, want 1 each", n, m)
	}
	if out := run(second, "fixture-cache-2"); !strings.Contains(out, "(cached)") {
		t.Errorf("an identical run was not served from the cache:\n%s", out)
	}
}

func TestEndToEndPanicDeliversTheResultsBeforeTheProcessDies(t *testing.T) {
	r := otlptest.NewReceiver(t)
	vars := exportedTo(r, "fixture-panic")
	// The customer's sampler applies to child spans only.
	vars["OTEL_TRACES_SAMPLER"] = "always_off"
	vars["BUILDKITE_ANALYTICS_LOCATION_PREFIX"] = "ignored/"
	// -trimpath replaces the module root with the module path in file names.
	out, code := goTest(t, "./panicky", vars, "-trimpath")
	if code == 0 || !strings.Contains(out, "panic: boom") {
		t.Fatalf("exit code = %d; want the panic to surface:\n%s", code, out)
	}
	spans := r.TestSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d test spans, want TestBeforePanic, TestPanics and TestPanics/child: %v", len(spans), otlptest.TestNames(spans))
	}
	for _, name := range []string{"TestPanics/child", "TestPanics"} {
		panicked := otlptest.FindSpan(t, spans, name)
		if got := otlptest.StringAttr(t, panicked.Attributes, "test.case.result.status"); got != "fail" {
			t.Errorf("%s status = %q, want fail", name, got)
		}
		if panicked.Status.GetMessage() != "test panicked" {
			t.Errorf("%s status message = %q", name, panicked.Status.GetMessage())
		}
		var exception *tracepb.Span_Event
		for _, ev := range panicked.Events {
			if ev.Name == "exception" {
				exception = ev
			}
		}
		if exception == nil {
			t.Fatalf("%s has no exception event: %v", name, panicked.Events)
		}
		if got := otlptest.StringAttr(t, exception.Attributes, "exception.type"); got != "panic" {
			t.Errorf("%s exception.type = %q", name, got)
		}
		if got := otlptest.StringAttr(t, exception.Attributes, "exception.stacktrace"); !strings.Contains(got, "panicky_test.go") {
			t.Errorf("%s exception.stacktrace does not reach the test:\n%s", name, got)
		}
		if got := otlptest.StringAttr(t, panicked.Attributes, "code.file.path"); got != "panicky/panicky_test.go" {
			t.Errorf("%s file = %q, want the module-relative path under -trimpath with the explicit empty prefix", name, got)
		}
	}
	if got := otlptest.StringAttr(t, otlptest.FindSpan(t, spans, "TestBeforePanic").Attributes, "test.case.result.status"); got != "pass" {
		t.Errorf("TestBeforePanic status = %q", got)
	}
	// Main never returns after a panic, so the export line comes from the
	// cleanup that flushed, once per flushed test.
	if !strings.Contains(out, collector.ExportedLine(3, fixtureModule+"/panicky")) {
		t.Errorf("go test output lacks the export line for the panicking package:\n%s", out)
	}
	if n := len(r.SpansOn(otlptest.StreamChild)); n != 0 {
		t.Errorf("OTEL_TRACES_SAMPLER=always_off should drop child spans, got %d", n)
	}
}

func TestEndToEndWithoutATokenIsANoOp(t *testing.T) {
	r := otlptest.NewReceiver(t)
	vars := exportedTo(r, "fixture-no-token")
	delete(vars, "BUILDKITE_TESTS_OTLP_TOKEN")
	out, code := goTest(t, ".", vars)
	if code != 1 {
		t.Fatalf("exit code = %d, want go test's own 1:\n%s", code, out)
	}
	if strings.Contains(out, "[buildkite-test-collector]") || strings.Contains(out, "buildkite-test-collector:") {
		t.Errorf("without a token the package must print nothing:\n%s", out)
	}
	if n := len(r.All()); n != 0 {
		t.Errorf("%d requests sent while disabled", n)
	}
}

// A token with a bad run key or endpoint is a misconfiguration worth one
// warning, unlike the silent no-token case.
func TestEndToEndMisconfigurationWarnsOnce(t *testing.T) {
	r := otlptest.NewReceiver(t)
	invalidEndpoint := exportedTo(r, "fixture-endpoint")
	invalidEndpoint["BUILDKITE_ANALYTICS_OTLP_ENDPOINT"] = "not a url"
	for _, tc := range []struct {
		name, warning string
		vars          map[string]string
	}{
		{"run key", "OpenTelemetry span export disabled: run key", exportedTo(r, "has a space")},
		{"endpoint", "OpenTelemetry span export disabled: endpoint", invalidEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := goTest(t, ".", tc.vars)
			if code != 1 {
				t.Fatalf("exit code = %d, want go test's own 1:\n%s", code, out)
			}
			if n := strings.Count(out, tc.warning); n != 1 {
				t.Errorf("disabled warning appeared %d times, want once:\n%s", n, out)
			}
			if n := len(r.All()); n != 0 {
				t.Errorf("%d requests sent while disabled", n)
			}
		})
	}
}

func TestEndToEndStartWithoutMainWarnsOnce(t *testing.T) {
	r := otlptest.NewReceiver(t)
	out, code := goTest(t, "./nomain", exportedTo(r, "fixture-nomain"))
	if code != 0 {
		t.Fatalf("exit code = %d:\n%s", code, out)
	}
	if n := strings.Count(out, "bktest.Main did not run"); n != 1 {
		t.Errorf("warning appeared %d times, want once:\n%s", n, out)
	}
	if n := len(r.All()); n != 0 {
		t.Errorf("%d requests sent without Main", n)
	}
}

func TestEndToEndMainPackageScopeIsTheImportPath(t *testing.T) {
	r := otlptest.NewReceiver(t)
	out, code := goTest(t, "./cmd/tool", exportedTo(r, "fixture-main"))
	if code != 0 {
		t.Fatalf("exit code = %d:\n%s", code, out)
	}
	spans := r.TestSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d test spans, want 1: %v", len(spans), otlptest.TestNames(spans))
	}
	s := otlptest.FindSpan(t, spans, "TestAnswer")
	if got := otlptest.StringAttr(t, s.Attributes, "buildkite.test.scope"); got != fixtureModule+"/cmd/tool" {
		t.Errorf("scope = %q, want the import path, not \"main\"", got)
	}
	if got := otlptest.StringAttr(t, s.Attributes, "code.file.path"); got != "cmd/tool/main_test.go" {
		t.Errorf("file = %q", got)
	}
	if want := collector.ExportedLine(1, fixtureModule+"/cmd/tool"); !strings.Contains(out, want) {
		t.Errorf("go test output lacks %q:\n%s", want, out)
	}
}
