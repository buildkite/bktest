package bktest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const fixtureModule = "example.com/fixture"

// goTest runs go test for the fixture package in a subprocess with a clean
// collector environment plus vars, and returns its combined output and exit
// code. The subprocess is the only place the collector meets the real
// testing package: TestMain, t.Cleanup ordering, panics and exit codes.
func goTest(t *testing.T, pkg string, vars map[string]string, flags ...string) (string, int) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go test in a subprocess")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append(append([]string{"test", "-count=1", "-v"}, flags...), pkg)...)
	cmd.Dir = filepath.Join("testdata", "fixture")
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); !collectorEnvVar(k) {
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

func exportedTo(r *receiver, runKey string) map[string]string {
	return map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "fixture-token",
		"BUILDKITE_ANALYTICS_KEY":           runKey,
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.endpoint(),
		// The SDK's own configuration must not reach the exporter.
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:9/",
		"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=garbage",
	}
}

// fixtureLine finds the line in fixture_test.go matching pattern,
// independently of the collector's own frame walking.
func fixtureLine(t *testing.T, pattern string) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "fixture", "fixture_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	loc := regexp.MustCompile("(?m)" + pattern).FindIndex(src)
	if loc == nil {
		t.Fatalf("%s not found in fixture_test.go", pattern)
	}
	return bytes.Count(src[:loc[0]], []byte("\n")) + 1
}

func intAttr(t *testing.T, attrs []*commonpb.KeyValue, key string) int {
	t.Helper()
	v, ok := attr(attrs, key)
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, keys(attrs))
	}
	return int(v.GetIntValue())
}

func TestEndToEndWithGoTest(t *testing.T) {
	r := newReceiver(t)
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

	// Locations point at the function that called Start: the test function
	// itself, or the subtest closure.
	fixtureLines := map[string]int{}
	for _, name := range []string{"TestPass", "TestFail", "TestSkip", "TestSubtests", "TestParallel", "TestTagged"} {
		fixtureLines[name] = fixtureLine(t, `^func `+name+`\(`)
	}
	fixtureLines["TestSubtests/a"] = fixtureLine(t, `t.Run\("a", func`)
	fixtureLines["TestSubtests/b"] = fixtureLine(t, `t.Run\("b", func`)
	fixtureLines["TestParallel/p1"] = fixtureLine(t, `t.Run\(name, func`)
	fixtureLines["TestParallel/p2"] = fixtureLines["TestParallel/p1"]

	spans := r.testSpans()
	want := map[string]string{
		"TestPass":        resultPass,
		"TestFail":        resultFail,
		"TestSkip":        resultSkipped,
		"TestSubtests":    resultFail,
		"TestSubtests/a":  resultPass,
		"TestSubtests/b":  resultFail,
		"TestParallel":    resultPass,
		"TestParallel/p1": resultPass,
		"TestParallel/p2": resultPass,
		"TestTagged":      resultPass,
	}
	if len(spans) != len(want) {
		t.Fatalf("got %d test spans, want %d: %v", len(spans), len(want), testNames(spans))
	}
	for name, status := range want {
		s := findSpan(t, spans, name)
		if got := stringAttr(t, s.Attributes, "test.case.result.status"); got != status {
			t.Errorf("%s status = %q, want %q", name, got, status)
		}
		if got := stringAttr(t, s.Attributes, attrTestScope); got != fixtureModule {
			t.Errorf("%s scope = %q, want %q", name, got, fixtureModule)
		}
		if got := stringAttr(t, s.Attributes, "code.file.path"); got != "svc/fixture_test.go" {
			t.Errorf("%s file = %q, want svc/fixture_test.go (BUILDKITE_ANALYTICS_LOCATION_PREFIX applied)", name, got)
		}
		if got, wantLine := intAttr(t, s.Attributes, "code.line.number"), fixtureLines[name]; got != wantLine {
			t.Errorf("%s line = %d, want %d (the function that called Start)", name, got, wantLine)
		}
		if got := stringAttr(t, s.Attributes, "test.case.name"); got != fixtureModule+"."+name {
			t.Errorf("%s test.case.name = %q", name, got)
		}
		if got := stringAttr(t, s.Attributes, tagAttributePrefix+"team"); got != "fixture" && name != "TestTagged" {
			t.Errorf("%s team tag = %q, want fixture", name, got)
		}
		if (s.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (status == resultFail) {
			t.Errorf("%s span status = %v for result %s", name, s.Status.GetCode(), status)
		}
	}

	pass := findSpan(t, spans, "TestPass")
	if d := time.Duration(pass.EndTimeUnixNano - pass.StartTimeUnixNano); d < 20*time.Millisecond || d > 10*time.Second {
		t.Errorf("TestPass duration = %s, want about 20ms", d)
	}

	tagged := findSpan(t, spans, "TestTagged")
	if got := stringAttr(t, tagged.Attributes, tagAttributePrefix+"team"); got != "overridden" {
		t.Errorf("per-test tag did not override the run tag: %q", got)
	}
	if got := stringAttr(t, tagged.Attributes, tagAttributePrefix+"flavour"); got != "vanilla" {
		t.Errorf("flavour tag = %q", got)
	}
	var annotated bool
	for _, ev := range tagged.Events {
		if ev.Name == annotationEventName {
			annotated = stringAttr(t, ev.Attributes, attrAnnotation) == "hello from the fixture"
		}
	}
	if !annotated {
		t.Errorf("TestTagged has no annotation event: %v", tagged.Events)
	}

	children := r.spansOn(streamChild)
	byName := map[string][]*tracepb.Span{}
	for _, s := range children {
		byName[s.Name] = append(byName[s.Name], s)
	}
	if n := len(byName["fixture.work"]); n != 1 {
		t.Fatalf("fixture.work forwarded %d times, want 1: %v", n, childNames(r))
	}
	work := byName["fixture.work"][0]
	if hexOf(work.TraceId) != hexOf(tagged.TraceId) {
		t.Errorf("fixture.work is not in TestTagged's trace")
	}
	var bodyOfTagged *tracepb.Span
	for _, b := range byName[bodyPhaseName] {
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
	for _, b := range byName[bodyPhaseName] {
		bodies++
		if hexOf(b.TraceId) == hexOf(pass.TraceId) {
			t.Errorf("TestPass has an empty body phase that should not export")
		}
	}
	if bodies != 4 { // TestFail, TestSubtests, TestSubtests/b, TestTagged
		t.Errorf("got %d body phases, want 4: %v", bodies, childNames(r))
	}

	for _, req := range r.all() {
		if got := req.Header.Get(runKeyHeader); got != "fixture-run" {
			t.Errorf("run key header = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != `Token token="fixture-token"` {
			t.Errorf("authorization = %q", got)
		}
	}
}

func TestEndToEndPanicDeliversTheResultsBeforeTheProcessDies(t *testing.T) {
	r := newReceiver(t)
	vars := exportedTo(r, "fixture-panic")
	// The customer's sampler applies to child spans only.
	vars["OTEL_TRACES_SAMPLER"] = "always_off"
	vars["BUILDKITE_ANALYTICS_LOCATION_PREFIX"] = "ignored/"
	// -trimpath replaces the module root with the module path in file names.
	out, code := goTest(t, "./panicky", vars, "-trimpath")
	if code == 0 || !strings.Contains(out, "panic: boom") {
		t.Fatalf("exit code = %d; want the panic to surface:\n%s", code, out)
	}
	spans := r.testSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d test spans, want TestBeforePanic, TestPanics and TestPanics/child: %v", len(spans), testNames(spans))
	}
	for _, name := range []string{"TestPanics/child", "TestPanics"} {
		panicked := findSpan(t, spans, name)
		if got := stringAttr(t, panicked.Attributes, "test.case.result.status"); got != resultFail {
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
		if got := stringAttr(t, exception.Attributes, "exception.type"); got != exceptionTypePanic {
			t.Errorf("%s exception.type = %q", name, got)
		}
		if got := stringAttr(t, exception.Attributes, "exception.stacktrace"); !strings.Contains(got, "panicky_test.go") {
			t.Errorf("%s exception.stacktrace does not reach the test:\n%s", name, got)
		}
		if got := stringAttr(t, panicked.Attributes, "code.file.path"); got != "panicky/panicky_test.go" {
			t.Errorf("%s file = %q, want the module-relative path under -trimpath with the explicit empty prefix", name, got)
		}
	}
	if got := stringAttr(t, findSpan(t, spans, "TestBeforePanic").Attributes, "test.case.result.status"); got != resultPass {
		t.Errorf("TestBeforePanic status = %q", got)
	}
	if n := len(r.spansOn(streamChild)); n != 0 {
		t.Errorf("OTEL_TRACES_SAMPLER=always_off should drop child spans, got %d", n)
	}
}

func TestEndToEndWithoutATokenIsANoOp(t *testing.T) {
	r := newReceiver(t)
	vars := exportedTo(r, "fixture-no-token")
	delete(vars, "BUILDKITE_TESTS_OTLP_TOKEN")
	out, code := goTest(t, ".", vars)
	if code != 1 {
		t.Fatalf("exit code = %d, want go test's own 1:\n%s", code, out)
	}
	if n := strings.Count(out, "OpenTelemetry span export disabled: no token configured"); n != 1 {
		t.Errorf("disabled warning appeared %d times, want once:\n%s", n, out)
	}
	if n := len(r.all()); n != 0 {
		t.Errorf("%d requests sent while disabled", n)
	}
}

func TestEndToEndStartWithoutMainWarnsOnce(t *testing.T) {
	r := newReceiver(t)
	out, code := goTest(t, "./nomain", exportedTo(r, "fixture-nomain"))
	if code != 0 {
		t.Fatalf("exit code = %d:\n%s", code, out)
	}
	if n := strings.Count(out, "bktest.Main did not run"); n != 1 {
		t.Errorf("warning appeared %d times, want once:\n%s", n, out)
	}
	if n := len(r.all()); n != 0 {
		t.Errorf("%d requests sent without Main", n)
	}
}

func testNames(spans []*tracepb.Span) []string {
	var names []string
	for _, s := range spans {
		if v, ok := attr(s.Attributes, attrTestName); ok {
			names = append(names, v.GetStringValue())
		} else {
			names = append(names, "<"+s.Name+" #"+strconv.Itoa(len(names))+">")
		}
	}
	return names
}
