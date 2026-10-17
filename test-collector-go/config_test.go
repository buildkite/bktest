package bktest

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestDisabledWithoutToken(t *testing.T) {
	out := captureWarnings(t)
	baseEnv(t, map[string]string{"BUILDKITE_ANALYTICS_KEY": "run-1", "BUILDKITE_TESTS_OTLP_TOKEN": "   "})

	c, err := configure(context.Background(), options{})
	if !errors.Is(err, errNoToken) || c != nil {
		t.Fatalf("configure = (%v, %v), want (nil, errNoToken)", c, err)
	}
	if setUp(options{}) != nil {
		t.Fatal("setUp returned a collector without a token")
	}
	if got := out.String(); !strings.Contains(got, "OpenTelemetry span export disabled: no token configured") || !strings.Contains(got, "not be uploaded") {
		t.Fatalf("warning = %q", got)
	}
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Fatalf("expected exactly one warning line, got %d:\n%s", n, out.String())
	}
}

func TestTokenPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		otlp, json string
		want       string
	}{
		{"relay token wins", "relay", "suite", "relay"},
		{"relay token is trimmed", "  relay \n", "suite", "relay"},
		{"blank relay token falls back", " \t", "suite", "suite"},
		{"json token alone", "", "suite", "suite"},
		{"nothing", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, map[string]string{"BUILDKITE_TESTS_OTLP_TOKEN": tc.otlp, "BUILDKITE_ANALYTICS_TOKEN": tc.json})
			if got := apiToken(); got != tc.want {
				t.Fatalf("apiToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEndpointResolution(t *testing.T) {
	baseEnv(t, nil)
	if got := endpoint(); got != "https://tests-otlp.buildkite.com/v1/traces" {
		t.Fatalf("default endpoint = %q", got)
	}
	t.Setenv("BUILDKITE_ANALYTICS_OTLP_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	if got := endpoint(); got != "http://127.0.0.1:4318/v1/traces" {
		t.Fatalf("override endpoint = %q", got)
	}

	for _, bad := range []string{"", "not a url", "ftp://x/v1/traces", "http://", "/v1/traces"} {
		if _, err := validateEndpoint(bad); err == nil {
			t.Errorf("validateEndpoint(%q) accepted", bad)
		}
	}
}

func TestInvalidEndpointDisablesExport(t *testing.T) {
	out := captureWarnings(t)
	baseEnv(t, map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "tok",
		"BUILDKITE_ANALYTICS_KEY":           "run-1",
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": "",
	})
	c := setUp(options{})
	if c != nil {
		t.Fatal("expected no collector for an empty endpoint")
	}
	if !strings.Contains(out.String(), "OpenTelemetry span export disabled: endpoint \"\"") {
		t.Fatalf("warning = %q", out.String())
	}
}

func TestRunKeyValidation(t *testing.T) {
	cases := map[string]bool{
		"":                               false,
		" ":                              false,
		"a":                              true,
		strings.Repeat("k", 255):         true,
		strings.Repeat("k", 256):         false,
		"build 42":                       false,
		"!~#$%^&*()_+-=[]{}|;':\",./<>?": true,
		"tab\there":                      false,
		"ünïcode":                        false,
		"0192d1a2-b3c4-7def-8012-3456789abcde-rspec": true,
	}
	for key, want := range cases {
		if got := validRunKey(key); got != want {
			t.Errorf("validRunKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestInvalidRunKeyDisablesExportBeforeBuildingExporters(t *testing.T) {
	out := captureWarnings(t)
	r := newReceiver(t)
	baseEnv(t, map[string]string{
		"BUILDKITE_TESTS_OTLP_TOKEN":        "tok",
		"BUILDKITE_ANALYTICS_KEY":           "has space",
		"BUILDKITE_ANALYTICS_OTLP_ENDPOINT": "://not-even-parsable",
	})
	c, err := configure(context.Background(), options{})
	if c != nil || err == nil {
		t.Fatalf("configure = (%v, %v), want (nil, error)", c, err)
	}
	want := `run key "has space" is invalid. Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); it must be 1-255 printable ASCII characters without spaces`
	if err.Error() != want {
		t.Fatalf("error = %q\nwant  %q", err, want)
	}
	if setUp(options{}) != nil {
		t.Fatal("setUp returned a collector for an invalid run key")
	}
	if !strings.Contains(out.String(), "OpenTelemetry span export disabled: run key \"has space\" is invalid") {
		t.Fatalf("warning = %q", out.String())
	}
	if len(r.all()) != 0 {
		t.Fatal("a request was sent despite the invalid run key")
	}
}

func TestSpanProcessorSizes(t *testing.T) {
	defaults := processorSizes{queue: 8192, batch: 120}
	cases := []struct {
		name         string
		queue, batch string
		want         processorSizes
		warn         string
	}{
		{"unset", "", "", defaults, ""},
		{"override", "5000", "100", processorSizes{5000, 100}, ""},
		{"leading zeros", "0100", "007", processorSizes{100, 7}, ""},
		{"max", "2147483647", "2147483647", processorSizes{2147483647, 2147483647}, ""},
		{"too large", "2147483648", "10", processorSizes{8192, 10}, "BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE must be a positive integer up to 2147483647, using default 8192"},
		{"zero", "0", "", processorSizes{8192, 120}, "QUEUE_SIZE must be a positive integer"},
		{"negative", "-5", "", defaults, "QUEUE_SIZE must be a positive integer"},
		{"decimal", "", "1.5", defaults, "BATCH_SIZE must be a positive integer up to 2147483647, using default 120"},
		{"whitespace", " 10", "", defaults, "QUEUE_SIZE must be a positive integer"},
		{"plus sign", "+10", "", defaults, "QUEUE_SIZE must be a positive integer"},
		{"batch above queue", "10", "20", defaults, "BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE must be <= BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE; using defaults (batch 120, queue 8192)"},
		{"batch equals queue", "10", "10", processorSizes{10, 10}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureWarnings(t)
			baseEnv(t, nil)
			if tc.queue != "" {
				t.Setenv("BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE", tc.queue)
			}
			if tc.batch != "" {
				t.Setenv("BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE", tc.batch)
			}
			got := spanProcessorSizes(defaultTestSpanProcessorEnvName, defaults)
			if got != tc.want {
				t.Fatalf("sizes = %+v, want %+v", got, tc.want)
			}
			if tc.warn == "" && out.Len() != 0 {
				t.Fatalf("unexpected warning %q", out.String())
			}
			if tc.warn != "" && !strings.Contains(out.String(), tc.warn) {
				t.Fatalf("warning = %q, want it to contain %q", out.String(), tc.warn)
			}
		})
	}
}

func TestSpanProcessorSizesAreIndependentPerStream(t *testing.T) {
	baseEnv(t, map[string]string{
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE": "3",
		"BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE": "2",
	})
	test := spanProcessorSizes(defaultTestSpanProcessorEnvName, processorSizes{queue: 8192, batch: 120})
	child := spanProcessorSizes(childSpanProcessorEnvName, processorSizes{queue: 2048, batch: 512})
	if test != (processorSizes{8192, 120}) {
		t.Fatalf("test sizes = %+v", test)
	}
	if child != (processorSizes{3, 2}) {
		t.Fatalf("child sizes = %+v", child)
	}
}

func TestRunEnvDetectionAndOverrides(t *testing.T) {
	t.Run("buildkite", func(t *testing.T) {
		baseEnv(t, map[string]string{
			"BUILDKITE_BUILD_ID":     "build-uuid",
			"BUILDKITE_BUILD_URL":    "https://buildkite.com/o/p/builds/1",
			"BUILDKITE_BRANCH":       "main",
			"BUILDKITE_COMMIT":       "abc",
			"BUILDKITE_BUILD_NUMBER": "1",
			"BUILDKITE_JOB_ID":       "job-uuid",
			"BUILDKITE_MESSAGE":      "msg",
		})
		got := detectRunEnv()
		want := runEnv{ci: "buildkite", key: "build-uuid", url: "https://buildkite.com/o/p/builds/1", branch: "main", commitSHA: "abc", number: "1", jobID: "job-uuid", message: "msg", pipelineRunID: "build-uuid", pipelineRunURL: "https://buildkite.com/o/p/builds/1"}
		if got != want {
			t.Fatalf("detectRunEnv() = %+v, want %+v", got, want)
		}
	})
	t.Run("analytics overrides win when non-empty", func(t *testing.T) {
		baseEnv(t, map[string]string{
			"BUILDKITE_BUILD_ID":         "build-uuid",
			"BUILDKITE_BRANCH":           "main",
			"BUILDKITE_ANALYTICS_KEY":    "build-uuid-otel",
			"BUILDKITE_ANALYTICS_BRANCH": "",
			"BUILDKITE_ANALYTICS_URL":    "https://example.com/run",
		})
		got := detectRunEnv()
		if got.key != "build-uuid-otel" || got.branch != "main" || got.url != "https://example.com/run" || got.pipelineRunID != "build-uuid" {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
	})
	t.Run("github actions", func(t *testing.T) {
		baseEnv(t, map[string]string{
			"GITHUB_RUN_NUMBER":  "7",
			"GITHUB_ACTION":      "__run",
			"GITHUB_RUN_ATTEMPT": "2",
			"GITHUB_RUN_ID":      "123",
			"GITHUB_REPOSITORY":  "acme/widgets",
			"GITHUB_REF_NAME":    "feature",
			"GITHUB_SHA":         "def",
		})
		got := detectRunEnv()
		if got.ci != "github_actions" || got.key != "__run-7-2" || got.url != "https://github.com/acme/widgets/actions/runs/123" || got.branch != "feature" || got.pipelineRunID != "123" || got.pipelineRunURL != got.url {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
	})
	t.Run("circleci", func(t *testing.T) {
		baseEnv(t, map[string]string{"CIRCLE_BUILD_NUM": "9", "CIRCLE_WORKFLOW_ID": "wf", "CIRCLE_BRANCH": "b", "CIRCLE_SHA1": "s", "CIRCLE_BUILD_URL": "u"})
		got := detectRunEnv()
		if got.ci != "circleci" || got.key != "wf-9" || got.url != "u" {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
	})
	t.Run("codeship", func(t *testing.T) {
		baseEnv(t, map[string]string{"CI_NAME": "codeship", "CI_BUILD_ID": "cs", "CI_BRANCH": "b", "CI_COMMIT_ID": "c", "CI_COMMIT_MESSAGE": "m"})
		got := detectRunEnv()
		if got.ci != "codeship" || got.key != "cs" || got.message != "m" {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
	})
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	t.Run("generic ci", func(t *testing.T) {
		baseEnv(t, map[string]string{"CI": "true"})
		got := detectRunEnv()
		if got.ci != "generic" || !uuid.MatchString(got.key) {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
	})
	t.Run("local", func(t *testing.T) {
		baseEnv(t, nil)
		got := detectRunEnv()
		if got.ci != "" || !uuid.MatchString(got.key) {
			t.Fatalf("detectRunEnv() = %+v", got)
		}
		if detectRunEnv().key == got.key {
			t.Fatal("local run keys should be random")
		}
	})
}
