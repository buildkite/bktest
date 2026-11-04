"""Span and resource attributes derived from the JSON data model."""

from dataclasses import replace

import pytest

from buildkite_test_collector.collector.payload import TestResultFailed
from buildkite_test_collector.collector.run_env import RunEnv, RunEnvBuilder
from buildkite_test_collector.pytest_plugin.otel import attributes as attrs


def buildkite_env(**extra):
    env = {
        "BUILDKITE_BUILD_ID": "build-1",
        "BUILDKITE_BUILD_URL": "https://buildkite.com/acme/app/builds/1",
        "BUILDKITE_BRANCH": "main",
        "BUILDKITE_COMMIT": "abc123",
        "BUILDKITE_ORGANIZATION_SLUG": "acme",
        "BUILDKITE_TEST_ENGINE_SUITE_SLUG": "app",
        "BUILDKITE_AGENT_ID": "agent-1",
        "BUILDKITE_STEP_ID": "step-1",
    }
    env.update(extra)
    return env


def test_producer_identity_lives_on_the_resource():
    env = buildkite_env()
    run_env = RunEnvBuilder(env).build()
    resource = attrs.producer_resource_attributes(env, run_env)
    assert resource["service.name"] == "app"
    assert resource["service.namespace"] == "acme"
    assert resource["cicd.pipeline.run.id"] == "build-1"
    assert resource["cicd.pipeline.run.url.full"] == "https://buildkite.com/acme/app/builds/1"
    assert resource["cicd.worker.id"] == "agent-1"
    assert resource["vcs.ref.head.name"] == "main"
    assert resource["vcs.ref.head.revision"] == "abc123"
    assert resource["vcs.ref.type"] == "branch"
    assert "buildkite.run_key" not in resource


def test_resource_uses_the_native_ci_run_id_not_the_run_key():
    env = buildkite_env(BUILDKITE_ANALYTICS_KEY="custom-key", BUILDKITE_TAG="v1")
    run_env = RunEnvBuilder(env).build()
    assert run_env.key == "custom-key"
    resource = attrs.producer_resource_attributes(env, run_env)
    assert resource["cicd.pipeline.run.id"] == "build-1"
    assert resource["vcs.ref.type"] == "tag"
    assert attrs.run_attributes(env, run_env)["buildkite.run_key"] == "custom-key"


def test_resource_omits_blank_worker_id_and_unknown_ci_run():
    env = buildkite_env(BUILDKITE_AGENT_ID="  ")
    resource = attrs.producer_resource_attributes(env, RunEnvBuilder(env).build())
    assert "cicd.worker.id" not in resource

    generic = RunEnvBuilder({}).build()
    resource = attrs.producer_resource_attributes({}, generic)
    assert "cicd.pipeline.run.id" not in resource
    assert "cicd.pipeline.run.url.full" not in resource
    assert "vcs.ref.type" not in resource


def test_github_actions_run_url_is_derived():
    env = {"GITHUB_ACTION": "x", "GITHUB_RUN_ID": "99", "GITHUB_REPOSITORY": "acme/app",
           "GITHUB_RUN_NUMBER": "9", "GITHUB_RUN_ATTEMPT": "1"}
    run_env = RunEnvBuilder(env).build()
    assert run_env.ci == "github_actions"
    assert attrs.ci_pipeline_run(env, run_env) == ("99", "https://github.com/acme/app/actions/runs/99")


def test_run_url_is_on_the_execution_only_when_it_differs_from_the_ci_url():
    env = buildkite_env()
    same = RunEnvBuilder(env).build()
    assert "buildkite.run_url" not in attrs.run_attributes(env, same)

    overridden = RunEnvBuilder(buildkite_env(BUILDKITE_ANALYTICS_URL="https://example.test/run")).build()
    assert attrs.run_attributes(env, overridden)["buildkite.run_url"] == "https://example.test/run"

    run_attrs = attrs.run_attributes(env, same)
    assert run_attrs["buildkite.step_id"] == "step-1"
    assert run_attrs["buildkite.test.framework.name"] == "pytest"
    assert run_attrs["buildkite.collector.name"]


def test_analytics_overrides_replace_detected_metadata():
    env = buildkite_env(
        BUILDKITE_ANALYTICS_BRANCH="feature",
        BUILDKITE_ANALYTICS_SHA="def456",
        BUILDKITE_ANALYTICS_NUMBER="42",
        BUILDKITE_ANALYTICS_JOB_ID="job-x",
        BUILDKITE_ANALYTICS_MESSAGE="hello",
        BUILDKITE_ANALYTICS_URL="",
    )
    run_env = RunEnvBuilder(env).build()
    assert (run_env.branch, run_env.commit_sha, run_env.number, run_env.job_id, run_env.message) \
        == ("feature", "def456", "42", "job-x", "hello")
    assert run_env.url == "https://buildkite.com/acme/app/builds/1"


def test_test_attributes_match_the_json_identity(successful_test):
    test_attrs = attrs.test_attributes(successful_test)
    assert test_attrs["buildkite.test.scope"] == "wyld stallyns"
    assert test_attrs["buildkite.test.name"] == "san dimas meltdown"
    assert test_attrs["test.case.name"] == "wyld stallyns::san dimas meltdown"
    assert test_attrs["test.suite.name"] == "wyld stallyns"
    assert test_attrs["code.file.path"] == "san_dimas_meltdown.py"
    assert test_attrs["code.line.number"] == 1
    assert test_attrs["buildkite.test.execution.external_id"] == str(successful_test.id)


def test_line_number_is_omitted_without_a_location_suffix(successful_test):
    no_line = replace(successful_test, location="san_dimas_meltdown.py")
    assert "code.line.number" not in attrs.test_attributes(no_line)
    no_location = replace(successful_test, location=None, file_name=None)
    test_attrs = attrs.test_attributes(no_location)
    assert "code.line.number" not in test_attrs
    assert "code.file.path" not in test_attrs
    empty_scope = replace(successful_test, scope="")
    assert attrs.test_attributes(empty_scope)["test.case.name"] == "san dimas meltdown"


def test_result_mapping(successful_test, failed_test, skipped_test, incomplete_test):
    assert attrs.result_status(successful_test) == "pass"
    assert attrs.result_status(failed_test) == "fail"
    assert attrs.result_status(skipped_test) == "skipped"
    assert attrs.result_status(incomplete_test) is None


def test_failure_reason_and_exception_events_are_capped(successful_test):
    failed = replace(successful_test, result=TestResultFailed(
        "r" * 2000,
        [
            {"expanded": ["m" * 20000], "backtrace": ["b" * 20000]},
            {"expanded": [], "backtrace": []},
            {"expanded": ["second"]},
        ],
    ))
    assert attrs.failure_reason(failed) == "r" * 1024
    events = attrs.exception_events(failed)
    assert len(events) == 2
    assert len(events[0]["exception.message"]) == 10243
    assert events[0]["exception.message"].endswith("...")
    assert len(events[0]["exception.stacktrace"]) == 16384
    assert events[1] == {"exception.message": "second"}
    assert attrs.failure_reason(successful_test) is None
    assert attrs.exception_events(successful_test) == []


def test_unencodable_text_is_scrubbed(successful_test):
    bad = replace(successful_test, name="bad\udc80name")
    assert attrs.test_attributes(bad)["buildkite.test.name"] == "bad\ufffdname"


def test_per_test_tags_override_run_tags(successful_test):
    tagged = replace(successful_test, tags={"ci.worker.id": "mine", "team": "x"})
    run_tags = attrs.tag_attributes({"ci.worker.id": "agent", "env": "ci"})
    merged = dict(run_tags)
    merged.update(attrs.test_tag_attributes(tagged))
    assert merged == {
        "buildkite.tag.ci.worker.id": "mine",
        "buildkite.tag.team": "x",
        "buildkite.tag.env": "ci",
    }


@pytest.mark.parametrize("value, limit, expected", [
    ("abc", 3, "abc"),
    ("abcd", 3, "..."),
    ("abcdef", 5, "ab..."),
])
def test_truncate_marks_the_cut(value, limit, expected):
    assert attrs.truncate(value, limit) == expected


def test_run_env_without_ci_has_no_native_run():
    run_env = RunEnv(ci="example", key="k", number=None, job_id=None, branch=None,
                     commit_sha=None, message=None, url="https://example.test")
    assert attrs.ci_pipeline_run({}, run_env) == (None, None)
    assert attrs.run_attributes({}, run_env)["buildkite.run_url"] == "https://example.test"
