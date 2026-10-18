package collector

import (
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const (
	attrExecutionVia    = "buildkite.execution.via"
	attrRunKey          = "buildkite.run_key"
	attrTestScope       = "buildkite.test.scope"
	attrTestName        = "buildkite.test.name"
	attrExternalID      = "buildkite.test.execution.external_id"
	attrAnnotation      = "buildkite.annotation"
	tagAttributePrefix  = "buildkite.tag."
	workerIDTag         = "ci.worker.id"
	executionVia        = "otlp"
	testSpanName        = "test.execution"
	bodyPhaseName       = "test.body"
	setupPhaseName      = "test.setup"
	teardownPhaseName   = "test.teardown"
	annotationEventName = "test.annotation"
)

// producerResource identifies the process emitting spans: the suite, the CI
// pipeline run, the worker and the VCS ref. The test provider always uses
// it; the child stream uses it only when the collector installs the global
// provider. Test Engine run metadata goes on each test span instead (see
// runAttributes).
func producerResource(env runEnv) *resource.Resource {
	attrs := []attribute.KeyValue{}
	add := func(key attribute.Key, value string) {
		if value != "" {
			attrs = append(attrs, key.String(value))
		}
	}
	add(semconv.ServiceNameKey, os.Getenv("BUILDKITE_TEST_ENGINE_SUITE_SLUG"))
	add(semconv.ServiceNamespaceKey, os.Getenv("BUILDKITE_ORGANIZATION_SLUG"))
	add(semconv.CICDPipelineRunIDKey, env.pipelineRunID)
	if env.pipelineRunID != "" {
		add(semconv.CICDPipelineRunURLFullKey, env.pipelineRunURL)
	}
	if worker := os.Getenv("BUILDKITE_AGENT_ID"); strings.TrimSpace(worker) != "" {
		add(semconv.CICDWorkerIDKey, worker)
	}
	add(semconv.ProcessRuntimeVersionKey, runtime.Version())
	add(semconv.VCSRefHeadNameKey, env.branch)
	add(semconv.VCSRefHeadRevisionKey, env.commitSHA)
	if env.branch != "" {
		refType := "branch"
		if os.Getenv("BUILDKITE_TAG") != "" {
			refType = "tag"
		}
		add(semconv.VCSRefTypeKey, refType)
	}

	merged, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		// Merge only fails on conflicting schema URLs, which a schemaless
		// resource cannot cause; keep ours rather than lose the identity.
		return resource.NewSchemaless(attrs...)
	}
	return merged
}

// runAttributes describe each test execution's run; they go on every test
// span so the server can read them without consulting the resource. Tags
// are not among them: each execution writes its merged tags last.
func runAttributes(env runEnv, locationPrefix string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	add(attrRunKey, env.key)
	add("buildkite.build_number", env.number)
	add("buildkite.job_id", env.jobID)
	add("buildkite.message", env.message)
	add("buildkite.step_id", os.Getenv("BUILDKITE_STEP_ID"))
	add("buildkite.collector.name", collectorName)
	add("buildkite.collector.version", Version)
	add("buildkite.location_prefix", locationPrefix)
	add("buildkite.test.framework.name", frameworkName)
	add("buildkite.test.framework.version", runtime.Version())
	if env.url != "" && env.url != env.pipelineRunURL {
		add("buildkite.run_url", env.url)
	}
	return attrs
}

// runTags are Options.Tags plus the automatic ci.worker.id; a configured tag
// with the same key wins.
func runTags(configured map[string]string) map[string]string {
	tags := map[string]string{}
	if worker := os.Getenv("BUILDKITE_AGENT_ID"); strings.TrimSpace(worker) != "" {
		tags[workerIDTag] = worker
	}
	maps.Copy(tags, configured)
	return tags
}

func tagAttributes(tags map[string]string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		attrs = append(attrs, attribute.String(tagAttributePrefix+k, tags[k]))
	}
	return attrs
}
