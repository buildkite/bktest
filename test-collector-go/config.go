package bktest

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultEndpoint = "https://tests-otlp.buildkite.com/v1/traces"

	collectorName = "go-buildkite-test-collector"
	frameworkName = "gotest"
	tracerName    = "buildkite-test-collector"

	runKeyHeader     = "Buildkite-Tests-Run-Key"
	spanStreamHeader = "Buildkite-Tests-Span-Stream"
	streamTest       = "test"
	streamChild      = "child"

	// processorTimeout bounds the shutdown after m.Run and is shared by
	// every panic-time flush.
	processorTimeout = 30 * time.Second
	exportTimeout    = 30 * time.Second

	testSpanQueueSize     = 8192
	testSpanBatchSize     = 120
	testSpanScheduleDelay = 1 * time.Second
	childSpanQueueSize    = 2048
	childSpanBatchSize    = 512
	childSpanDelay        = 5 * time.Second
	spanProcessorSizeMax  = 1<<31 - 1

	testSpanAttributeCountLimit     = 128
	testSpanAttributeLengthLimit    = 10243 // the limit the other collectors use
	testSpanEventCountLimit         = 100
	testSpanEventAttributeLength    = 16384
	testSpanEventAttributeCountMax  = 128
	defaultTestSpanProcessorEnvName = "BUILDKITE_TESTS_OTEL_TEST_SPAN"
	childSpanProcessorEnvName       = "BUILDKITE_TESTS_OTEL_CHILD_SPAN"
)

var runKeyFormat = regexp.MustCompile(`^[!-~]{1,255}$`)

// runEnv is the resolved CI metadata for this run: detected CI values,
// overridden by non-empty BUILDKITE_ANALYTICS_* values. pipelineRunID and
// pipelineRunURL are the CI provider's own identity for the producer
// resource; the Test Engine run key is a separate identity on the test span
// and BUILDKITE_ANALYTICS_* cannot override them.
type runEnv struct {
	ci             string // "buildkite", "github_actions", "circleci", "codeship", "generic" or ""
	key            string
	url            string
	branch         string
	commitSHA      string
	number         string
	jobID          string
	message        string
	pipelineRunID  string
	pipelineRunURL string
}

func detectRunEnv() runEnv {
	env := ciRunEnv()
	override := func(dst *string, name string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	override(&env.key, "BUILDKITE_ANALYTICS_KEY")
	override(&env.url, "BUILDKITE_ANALYTICS_URL")
	override(&env.branch, "BUILDKITE_ANALYTICS_BRANCH")
	override(&env.commitSHA, "BUILDKITE_ANALYTICS_SHA")
	override(&env.number, "BUILDKITE_ANALYTICS_NUMBER")
	override(&env.jobID, "BUILDKITE_ANALYTICS_JOB_ID")
	override(&env.message, "BUILDKITE_ANALYTICS_MESSAGE")
	return env
}

func ciRunEnv() runEnv {
	switch {
	case os.Getenv("BUILDKITE_BUILD_ID") != "":
		return runEnv{
			ci:             "buildkite",
			key:            os.Getenv("BUILDKITE_BUILD_ID"),
			url:            os.Getenv("BUILDKITE_BUILD_URL"),
			branch:         os.Getenv("BUILDKITE_BRANCH"),
			commitSHA:      os.Getenv("BUILDKITE_COMMIT"),
			number:         os.Getenv("BUILDKITE_BUILD_NUMBER"),
			jobID:          os.Getenv("BUILDKITE_JOB_ID"),
			message:        os.Getenv("BUILDKITE_MESSAGE"),
			pipelineRunID:  os.Getenv("BUILDKITE_BUILD_ID"),
			pipelineRunURL: os.Getenv("BUILDKITE_BUILD_URL"),
		}
	case os.Getenv("GITHUB_RUN_NUMBER") != "":
		runID, repository := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_REPOSITORY")
		runURL := ""
		if runID != "" && repository != "" {
			runURL = "https://github.com/" + repository + "/actions/runs/" + runID
		}
		return runEnv{
			ci:             "github_actions",
			key:            os.Getenv("GITHUB_ACTION") + "-" + os.Getenv("GITHUB_RUN_NUMBER") + "-" + os.Getenv("GITHUB_RUN_ATTEMPT"),
			url:            runURL,
			branch:         os.Getenv("GITHUB_REF_NAME"),
			commitSHA:      os.Getenv("GITHUB_SHA"),
			number:         os.Getenv("GITHUB_RUN_NUMBER"),
			pipelineRunID:  runID,
			pipelineRunURL: runURL,
		}
	case os.Getenv("CIRCLE_BUILD_NUM") != "":
		return runEnv{
			ci:            "circleci",
			key:           os.Getenv("CIRCLE_WORKFLOW_ID") + "-" + os.Getenv("CIRCLE_BUILD_NUM"),
			url:           os.Getenv("CIRCLE_BUILD_URL"),
			branch:        os.Getenv("CIRCLE_BRANCH"),
			commitSHA:     os.Getenv("CIRCLE_SHA1"),
			number:        os.Getenv("CIRCLE_BUILD_NUM"),
			pipelineRunID: os.Getenv("CIRCLE_WORKFLOW_ID"),
		}
	case os.Getenv("CI_NAME") == "codeship":
		return runEnv{
			ci:            "codeship",
			key:           os.Getenv("CI_BUILD_ID"),
			url:           os.Getenv("CI_PULL_REQUEST"),
			branch:        os.Getenv("CI_BRANCH"),
			commitSHA:     os.Getenv("CI_COMMIT_ID"),
			message:       os.Getenv("CI_COMMIT_MESSAGE"),
			pipelineRunID: os.Getenv("CI_BUILD_ID"),
		}
	case os.Getenv("CI") != "":
		return runEnv{ci: "generic", key: uuid.NewString()}
	default:
		return runEnv{key: uuid.NewString()}
	}
}

func apiToken() string {
	if token := strings.TrimSpace(os.Getenv("BUILDKITE_TESTS_OTLP_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("BUILDKITE_ANALYTICS_TOKEN"))
}

func endpoint() string {
	if v, ok := os.LookupEnv("BUILDKITE_ANALYTICS_OTLP_ENDPOINT"); ok {
		return v
	}
	return defaultEndpoint
}

// validateEndpoint rejects what WithEndpointURL would quietly mishandle: an
// unparsable URL falls back to the OTEL_EXPORTER_OTLP_* or localhost default,
// and any scheme other than https is sent as plain http.
func validateEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q is invalid: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint %q must be an http or https URL", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("endpoint %q has no host", raw)
	}
	return u, nil
}

func validRunKey(key string) bool {
	return runKeyFormat.MatchString(key)
}

type processorSizes struct {
	queue int
	batch int
}

// spanProcessorSizes resolves the queue and batch size for one stream from
// <prefix>_QUEUE_SIZE and <prefix>_BATCH_SIZE, warning about and replacing
// values the batch processor could not use.
func spanProcessorSizes(prefix string, defaults processorSizes) processorSizes {
	queueName := prefix + "_QUEUE_SIZE"
	batchName := prefix + "_BATCH_SIZE"
	sizes := processorSizes{
		queue: spanProcessorSize(queueName, defaults.queue),
		batch: spanProcessorSize(batchName, defaults.batch),
	}
	if sizes.batch > sizes.queue {
		warnf("%s must be <= %s; using defaults (batch %d, queue %d)", batchName, queueName, defaults.batch, defaults.queue)
		return defaults
	}
	return sizes
}

func spanProcessorSize(name string, def int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	if n, err := strconv.ParseUint(value, 10, 31); err == nil && n >= 1 {
		return int(n)
	}
	warnf("%s must be a positive integer up to %d, using default %d", name, spanProcessorSizeMax, def)
	return def
}
