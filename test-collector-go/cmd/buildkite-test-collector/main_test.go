package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/bktest/test-collector-go/internal/collector"
	"github.com/buildkite/bktest/test-collector-go/internal/otlptest"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const pkg = "example.com/t2j/p"

// ev builds one go test -json line for pkg.
func ev(action, test, output string, extra ...string) string {
	s := fmt.Sprintf(`{"Time":"2026-10-18T05:00:00Z","Action":%q,"Package":%q`, action, pkg)
	if test != "" {
		s += fmt.Sprintf(`,"Test":%q`, test)
	}
	if output != "" {
		s += fmt.Sprintf(`,"Output":%q`, output)
	}
	for _, e := range extra {
		s += "," + e
	}
	return s + "}\n"
}

// oneFailingStream is a package with a passing and a failing test.
var oneFailingStream = strings.Join([]string{
	ev("start", "", ""),
	ev("run", "TestPass", ""),
	ev("output", "TestPass", "=== RUN   TestPass\n"),
	ev("output", "TestPass", "--- PASS: TestPass (0.00s)\n"),
	ev("pass", "TestPass", "", `"Elapsed":0`),
	ev("run", "TestFail", ""),
	ev("output", "TestFail", "=== RUN   TestFail\n"),
	ev("output", "TestFail", "    p_test.go:17: deliberate failure\n"),
	ev("output", "TestFail", "--- FAIL: TestFail (0.00s)\n"),
	ev("fail", "TestFail", "", `"Elapsed":0`),
	ev("output", "", "FAIL\n"),
	ev("output", "", "FAIL\texample.com/t2j/p\t0.227s\n"),
	ev("fail", "", "", `"Elapsed":0.227`),
}, "")

var passingStream = strings.Join([]string{
	ev("run", "TestPass", ""),
	ev("output", "TestPass", "=== RUN   TestPass\n"),
	ev("output", "TestPass", "--- PASS: TestPass (0.00s)\n"),
	ev("pass", "TestPass", "", `"Elapsed":0`),
	ev("output", "", "ok  \texample.com/t2j/p\t0.1s\n"),
	ev("pass", "", "", `"Elapsed":0.1`),
}, "")

// configure gives the wrapper a token, a run key and the receiver as its
// endpoint, with every other collector variable cleared.
func configure(t *testing.T, r *otlptest.Receiver, env map[string]string) {
	t.Helper()
	vars := map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "secret-token",
		"BUILDKITE_ANALYTICS_KEY":           "run-key-" + strings.ReplaceAll(t.Name(), "/", "_"),
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": r.Endpoint(),
	}
	for k, v := range env {
		vars[k] = v
	}
	otlptest.ClearEnv(t, vars)
}

func runWrapper(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(strings.NewReader(stdin), &out, &errOut, args)
	return code, out.String(), errOut.String()
}

func TestExportsEachTestAndMirrorsTheFailure(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	code, stdout, stderr := runWrapper(t, oneFailingStream, "-tag", "team=payments")

	if code != 1 {
		t.Errorf("exit = %d, want 1 for a failing test\n%s", code, stderr)
	}
	if stdout != "=== RUN   TestPass\n--- PASS: TestPass (0.00s)\n=== RUN   TestFail\n    p_test.go:17: deliberate failure\n--- FAIL: TestFail (0.00s)\nFAIL\nFAIL\texample.com/t2j/p\t0.227s\n" {
		t.Errorf("stdout is not the go test -v output:\n%q", stdout)
	}
	if strings.Contains(stderr, "TEST RESULTS MISSING") || strings.Contains(stderr, "disabled") {
		t.Errorf("stderr:\n%s", stderr)
	}

	spans := r.TestSpans()
	if got := otlptest.TestNames(spans); !equalSets(got, []string{"TestPass", "TestFail"}) {
		t.Fatalf("exported %v", got)
	}
	if len(r.Stream(otlptest.StreamChild)) != 0 {
		t.Errorf("wrapper mode must not send child spans")
	}
	for _, req := range r.All() {
		if got := req.Header.Get(otlptest.RunKeyHeader); got != os.Getenv("BUILDKITE_ANALYTICS_KEY") {
			t.Errorf("run key header = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != `Token token="secret-token"` {
			t.Errorf("authorization = %q", got)
		}
	}

	fail := otlptest.FindSpan(t, spans, "TestFail")
	if fail.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || fail.Status.GetMessage() != "p_test.go:17: deliberate failure" {
		t.Errorf("status = %v", fail.Status)
	}
	if otlptest.StringAttr(t, fail.Attributes, "buildkite.test.scope") != pkg || otlptest.StringAttr(t, fail.Attributes, "buildkite.tag.team") != "payments" {
		t.Errorf("attributes = %v", fail.Attributes)
	}
	if otlptest.StringAttr(t, fail.Attributes, string(semconv.TestCaseResultStatusKey)) != "fail" {
		t.Errorf("result = %v", fail.Attributes)
	}
	if len(fail.Events) != 1 || fail.Events[0].Name != "exception" {
		t.Errorf("events = %v", fail.Events)
	}
	if _, ok := otlptest.Attr(fail.Attributes, string(semconv.CodeFilePathKey)); ok {
		t.Errorf("a package outside the current module must carry no location: %v", fail.Attributes)
	}
}

func TestQuietKeepsStdoutEmptyAndChangesNothingElse(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, map[string]string{"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": otlptest.ClosedPort(t) + "/v1/traces"})

	code, stdout, stderr := runWrapper(t, oneFailingStream, "-quiet")

	if code != 1 {
		t.Errorf("exit = %d, want 1 for a failing test", code)
	}
	if stdout != "" {
		t.Errorf("-quiet must print nothing on stdout:\n%q", stdout)
	}
	if !strings.Contains(stderr, "TEST RESULTS MISSING: OpenTelemetry dropped 2 test.execution span(s)") {
		t.Errorf("warnings must still reach stderr:\n%s", stderr)
	}
}

func TestNoTokenWarnsOnceAndStillMirrorsTheTests(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, map[string]string{"BUILDKITE_TESTS_OTLP_TOKEN": "", "BUILDKITE_ANALYTICS_TOKEN": ""})

	code, stdout, stderr := runWrapper(t, oneFailingStream)

	if code != 1 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(stdout, "--- FAIL: TestFail") {
		t.Errorf("output not streamed:\n%s", stdout)
	}
	if strings.Count(stderr, "OpenTelemetry span export disabled: no token configured") != 1 || strings.Count(stderr, "[buildkite-test-collector]") != 1 {
		t.Errorf("stderr:\n%s", stderr)
	}
	if len(r.All()) != 0 {
		t.Errorf("exported without a token")
	}
}

func TestUnreachableEndpointWarnsResultsMissingAndExitsWithTheTestsStatus(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, map[string]string{"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": otlptest.ClosedPort(t) + "/v1/traces"})

	started := time.Now()
	code, _, stderr := runWrapper(t, passingStream)

	if code != 0 {
		t.Errorf("exit = %d, want the tests' own status", code)
	}
	if !strings.Contains(stderr, "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s)") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("took %s, over the flush budget", took)
	}
}

func TestInputWithoutJSONEventsExitsTwo(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	plain := "=== RUN   TestPass\n--- PASS: TestPass (0.00s)\nPASS\nok  \texample.com/t2j/p\t0.1s\n"
	code, stdout, stderr := runWrapper(t, plain)

	if code != 2 {
		t.Errorf("exit = %d", code)
	}
	if stdout != plain {
		t.Errorf("input must pass through verbatim:\n%q", stdout)
	}
	if !strings.Contains(stderr, "no go test -json events on stdin") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestUnexpectedArgumentsAndBadTagsExitTwo(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	if code, _, stderr := runWrapper(t, passingStream, "./..."); code != 2 || !strings.Contains(stderr, `unexpected argument "./..."`) {
		t.Errorf("exit = %d\n%s", code, stderr)
	}
	if code, _, stderr := runWrapper(t, passingStream, "-tag", "nokey"); code != 2 || !strings.Contains(stderr, "want key=value") {
		t.Errorf("exit = %d\n%s", code, stderr)
	}
}

func TestWarnsWhenTheLibraryAlsoExportedAPackage(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	stream := passingStream + ev("output", "", collector.ExportedLine(1, pkg)+"\n")
	code, _, stderr := runWrapper(t, stream)

	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(stderr, "package example.com/t2j/p was also exported by the bktest library; results are duplicated") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if n := len(r.TestSpans()); n != 1 {
		t.Errorf("the wrapper still exports its own copy: %d spans", n)
	}
}

func TestBuildFailureWarnsNamingThePackageAndExitsOne(t *testing.T) {
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	stream := ev("start", "", "") +
		ev("output", "", "FAIL\texample.com/t2j/p [build failed]\n") +
		ev("fail", "", "", `"Elapsed":0,"FailedBuild":"example.com/t2j/p [example.com/t2j/p.test]"`)
	code, _, stderr := runWrapper(t, stream)

	if code != 1 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(stderr, "package example.com/t2j/p failed before running any tests") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if len(r.TestSpans()) != 0 {
		t.Errorf("exported spans for a package that never ran")
	}
}

// goTestJSON runs go test -json for a fixture package in a subprocess, from
// the fixture's directory as a customer would, with the collector's
// variables cleared so the fixture's own TestMain does not export.
func goTestJSON(t *testing.T, pkg string, flags ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go test in a subprocess")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append(append([]string{"test", "-json", "-count=1"}, flags...), pkg)...)
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); !otlptest.CollectorEnvVar(k) {
			cmd.Env = append(cmd.Env, e)
		}
	}
	stream, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("go test -json: %v", err)
	}
	return string(stream)
}

// TestRealGoTestStreamResolvesLocations pipes a real go test -json run of
// the fixture module through the wrapper from inside that module, so go
// list can find the test files.
func TestRealGoTestStreamResolvesLocations(t *testing.T) {
	stream := goTestJSON(t, ".")
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	code, stdout, stderr := runWrapper(t, stream)

	if code != 1 {
		t.Errorf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "--- FAIL: TestFail") || !strings.Contains(stdout, "--- SKIP: TestSkip") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "buildkite-test-collector") {
		t.Errorf("the library must stay silent without a token:\n%s", stdout)
	}
	if strings.Contains(stderr, "duplicated") || strings.Contains(stderr, "TEST RESULTS MISSING") {
		t.Errorf("stderr:\n%s", stderr)
	}

	spans := r.TestSpans()
	want := []string{"TestPass", "TestFail", "TestSkip", "TestSubtests", "TestSubtests/a", "TestSubtests/b", "TestParallel", "TestParallel/p1", "TestParallel/p2", "TestTagged", "TestViaHelper", "TestViaHelper/case", "TestHelperSubtests", "TestHelperSubtests/case", "TestHelperSubtests/inline", "TestCleanupBeforeStart", "TestCleanupBeforeStart/sub", "TestWithoutStart"}
	if got := otlptest.TestNames(spans); !equalSets(got, want) {
		t.Fatalf("exported %v\nwant %v", got, want)
	}

	pass := otlptest.FindSpan(t, spans, "TestPass")
	if otlptest.StringAttr(t, pass.Attributes, "buildkite.test.scope") != "example.com/fixture" {
		t.Errorf("scope = %v", pass.Attributes)
	}
	if otlptest.StringAttr(t, pass.Attributes, string(semconv.CodeFilePathKey)) != "fixture_test.go" || otlptest.IntAttr(t, pass.Attributes, string(semconv.CodeLineNumberKey)) != 21 {
		t.Errorf("location = %v", pass.Attributes)
	}
	if d := time.Duration(pass.EndTimeUnixNano - pass.StartTimeUnixNano); d < 20*time.Millisecond || d > 5*time.Second {
		t.Errorf("TestPass duration = %s, want its Elapsed", d)
	}

	sub := otlptest.FindSpan(t, spans, "TestSubtests/b")
	if otlptest.IntAttr(t, sub.Attributes, string(semconv.CodeLineNumberKey)) != 36 {
		t.Errorf("subtest location = %v, want the parent's", sub.Attributes)
	}
	if otlptest.StringAttr(t, sub.Attributes, string(semconv.TestCaseResultStatusKey)) != "fail" {
		t.Errorf("subtest result = %v", sub.Attributes)
	}
	if otlptest.StringAttr(t, otlptest.FindSpan(t, spans, "TestSkip").Attributes, string(semconv.TestCaseResultStatusKey)) != "skipped" {
		t.Errorf("skip not mapped to skipped")
	}
}

// A package behind a build tag has locations only when the wrapper lists
// it with the same tags go test compiled it with.
func TestBuildTagsFlagGivesTaggedPackagesTheirLocations(t *testing.T) {
	stream := goTestJSON(t, "./tagged", "-tags=integration")
	r := otlptest.NewReceiver(t)
	configure(t, r, nil)

	if code, _, stderr := runWrapper(t, stream, "-tags=integration"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	spans := r.TestSpans()
	if got := otlptest.TestNames(spans); !equalSets(got, []string{"TestTagged", "TestTagged/sub", "TestTagged/helper"}) {
		t.Fatalf("exported %v", got)
	}
	line := declarationLine(t, filepath.Join("..", "..", "testdata", "fixture", "tagged", "tagged_test.go"), "TestTagged")
	for _, s := range spans {
		if otlptest.StringAttr(t, s.Attributes, string(semconv.CodeFilePathKey)) != "tagged/tagged_test.go" || otlptest.IntAttr(t, s.Attributes, string(semconv.CodeLineNumberKey)) != line {
			t.Errorf("location = %v, want tagged/tagged_test.go:%d", s.Attributes, line)
		}
	}
}

// declarationLine is the line "func <name>(" is declared on in the file,
// read independently of the collector's own parsing.
func declarationLine(t *testing.T, file, name string) int {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(l, "func "+name+"(") {
			return i + 1
		}
	}
	t.Fatalf("func %s not declared in %s", name, file)
	return 0
}

func equalSets(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}
