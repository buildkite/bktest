import sys
from random import randint
from typing import Dict, Optional
from uuid import uuid4, UUID

import pytest

from buildkite_test_collector.collector.constants import VERSION
from buildkite_test_collector.collector.run_env import RunEnvBuilder


def test_detect_env_with_buildkite_api_env_vars_returns_the_correct_environment():
    id = str(uuid4())
    commit = uuid4().hex
    number = str(randint(0, 1000))
    job_id = str(randint(0, 1000))
    run_env = RunEnvBuilder({
        "BUILDKITE_BUILD_ID": id,
        "BUILDKITE_BUILD_URL": "https://example.test/buildkite",
        "BUILDKITE_BRANCH": "rufus",
        "BUILDKITE_COMMIT": commit,
        "BUILDKITE_BUILD_NUMBER": number,
        "BUILDKITE_JOB_ID": job_id,
        "BUILDKITE_MESSAGE": "All we are is dust in the wind, dude.",
    }).build()

    assert run_env.ci == "buildkite"
    assert run_env.key == id
    assert run_env.url == "https://example.test/buildkite"
    assert run_env.branch == "rufus"
    assert run_env.commit_sha == commit
    assert run_env.number == number
    assert run_env.job_id == job_id
    assert run_env.message == "All we are is dust in the wind, dude."


def test_detect_env_with_github_actions_env_vars_returns_the_correct_environment():
    run_number = str(randint(0, 1000))
    run_attempt = str(randint(0, 1000))
    run_id = str(uuid4())
    commit = uuid4().hex
    run_env = RunEnvBuilder({
        "GITHUB_ACTION": "bring-about-world-peace",
        "GITHUB_RUN_NUMBER": run_number,
        "GITHUB_RUN_ATTEMPT": run_attempt,
        "GITHUB_REPOSITORY": "bill-and-ted/phone-booth",
        "GITHUB_RUN_ID": run_id,
        "GITHUB_REF": "rufus",
        "GITHUB_SHA": commit,
        "TEST_ANALYTICS_COMMIT_MESSAGE": "excellent adventure"
    }).build()

    assert run_env.ci == "github_actions"
    assert run_env.key == f"bring-about-world-peace-{run_number}-{run_attempt}"
    assert run_env.url == f"https://github.com/bill-and-ted/phone-booth/actions/runs/{run_id}"
    assert run_env.branch == "rufus"
    assert run_env.commit_sha == commit
    assert run_env.number == run_number
    assert run_env.job_id is None
    assert run_env.message == "excellent adventure"

def test_detect_env_with_github_actions_missing_repository_or_run_id_omits_url():
    run_env = RunEnvBuilder({
        "GITHUB_ACTION": "bring-about-world-peace",
        "GITHUB_RUN_NUMBER": "42",
        "GITHUB_RUN_ATTEMPT": "1",
    }).build()

    assert run_env.ci == "github_actions"
    assert run_env.url is None
    assert "url" not in run_env.as_json()


def test_detect_env_with_circle_ci_env_vars_returns_the_correct_environment():
    build_num = str(randint(0, 1000))
    workflow_id = str(uuid4())
    commit = uuid4().hex
    run_env = RunEnvBuilder({
        "CIRCLE_BUILD_NUM": build_num,
        "CIRCLE_WORKFLOW_ID": workflow_id,
        "CIRCLE_BUILD_URL": "https://example.test/circle",
        "CIRCLE_BRANCH": "rufus",
        "CIRCLE_SHA1": commit,
        "TEST_ANALYTICS_COMMIT_MESSAGE": "excellent adventure"
    }).build()

    assert run_env.ci == "circleci"
    assert run_env.key == f"{workflow_id}-{build_num}"
    assert run_env.url == "https://example.test/circle"
    assert run_env.branch == "rufus"
    assert run_env.commit_sha == commit
    assert run_env.number == build_num
    assert run_env.job_id is None
    assert run_env.message == "excellent adventure"

def test_detect_env_with_generic_env_vars():
    run_env = RunEnvBuilder({}).build()

    assert run_env.ci == "generic"
    assert UUID(run_env.key)
    assert run_env.url is None
    assert run_env.branch is None
    assert run_env.commit_sha is None
    assert run_env.number is None
    assert run_env.job_id is None
    assert run_env.message is None


@pytest.fixture(params=[
    {"BUILDKITE_BUILD_ID": "build-id", "BUILDKITE_BUILD_URL": "https://example.test/build",
     "BUILDKITE_BRANCH": "main", "BUILDKITE_COMMIT": "abc123",
     "BUILDKITE_BUILD_NUMBER": "42", "BUILDKITE_JOB_ID": "job-id",
     "BUILDKITE_MESSAGE": "detected message"},
    {"GITHUB_ACTION": "test", "GITHUB_RUN_NUMBER": "43", "GITHUB_RUN_ATTEMPT": "2",
     "GITHUB_REPOSITORY": "org/repo", "GITHUB_RUN_ID": "123",
     "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": "def456",
     "TEST_ANALYTICS_COMMIT_MESSAGE": "detected message"},
    {"CIRCLE_BUILD_NUM": "44", "CIRCLE_WORKFLOW_ID": "workflow-id",
     "CIRCLE_BUILD_URL": "https://example.test/circle", "CIRCLE_BRANCH": "main",
     "CIRCLE_SHA1": "fed321", "TEST_ANALYTICS_COMMIT_MESSAGE": "detected message"},
    {},
], ids=["buildkite", "github-actions", "circleci", "generic"])
def ci_env(request: pytest.FixtureRequest) -> Dict[str, Optional[str]]:
    return request.param.copy()


def test_nonempty_analytics_overrides_reach_run_json(ci_env: Dict[str, Optional[str]]) -> None:
    detected_ci = RunEnvBuilder(ci_env).build().ci
    ci_env.update({
        "BUILDKITE_ANALYTICS_KEY": "custom-run",
        "BUILDKITE_ANALYTICS_URL": "https://example.test/custom-run",
        "BUILDKITE_ANALYTICS_BRANCH": "custom-branch",
        "BUILDKITE_ANALYTICS_SHA": "987abc",
        "BUILDKITE_ANALYTICS_NUMBER": "0",
        "BUILDKITE_ANALYTICS_JOB_ID": "custom-job",
        "BUILDKITE_ANALYTICS_MESSAGE": "custom message",
    })

    metadata = RunEnvBuilder(ci_env).build().as_json()

    assert metadata.items() >= {
        "CI": detected_ci,
        "key": "custom-run",
        "url": "https://example.test/custom-run",
        "branch": "custom-branch",
        "commit_sha": "987abc",
        "number": "0",
        "job_id": "custom-job",
        "message": "custom message",
    }.items()


def test_key_override_preserves_other_metadata(ci_env: Dict[str, Optional[str]]) -> None:
    expected = RunEnvBuilder(ci_env).build().as_json()
    expected["key"] = "custom-run"
    ci_env["BUILDKITE_ANALYTICS_KEY"] = "custom-run"

    assert RunEnvBuilder(ci_env).build().as_json() == expected


def test_metadata_override_does_not_require_a_key() -> None:
    metadata = RunEnvBuilder({
        "BUILDKITE_ANALYTICS_KEY": "",
        "BUILDKITE_ANALYTICS_BRANCH": "custom-branch",
    }).build().as_json()

    assert metadata["CI"] == "generic"
    assert UUID(metadata["key"]).version == 4
    assert metadata["branch"] == "custom-branch"
    assert "url" not in metadata


@pytest.mark.parametrize("value", [None, ""])
def test_empty_analytics_overrides_preserve_detected_metadata(
    ci_env: Dict[str, Optional[str]], value: Optional[str]
) -> None:
    expected = RunEnvBuilder(ci_env).build().as_json()
    ci_env.update({
        f"BUILDKITE_ANALYTICS_{suffix}": value
        for suffix in ("KEY", "URL", "BRANCH", "SHA", "NUMBER", "JOB_ID", "MESSAGE")
    })

    metadata = RunEnvBuilder(ci_env).build().as_json()

    if expected["CI"] == "generic":
        assert UUID(metadata["key"]).version == 4
        del expected["key"]
        del metadata["key"]
    assert metadata == expected


def test_env_as_json(fake_env):
    json = fake_env.as_json()

    assert json["CI"] == fake_env.ci
    assert json["key"] == fake_env.key
    assert json["number"] == fake_env.number
    assert json["job_id"] == fake_env.job_id
    assert json["branch"] == fake_env.branch
    assert json["commit_sha"] == fake_env.commit_sha
    assert json["message"] == fake_env.message
    assert json["url"] == fake_env.url
    assert json["collector"] == 'python-buildkite-test-collector'
    assert json["version"] == VERSION
    expected_version = f"{sys.version_info.major}.{sys.version_info.minor}.{sys.version_info.micro}"
    assert json["language_version"] == expected_version
    assert json["test_runner"] == "pytest"

def test_worker_id_tag_with_agent_id():
    tag = RunEnvBuilder({"BUILDKITE_AGENT_ID": "agent-123"}).worker_id_tag()

    assert tag == {"ci.worker.id": "agent-123"}

def test_worker_id_tag_without_agent_id():
    tag = RunEnvBuilder({}).worker_id_tag()

    assert tag == {}

def test_worker_id_tag_with_blank_agent_id():
    tag = RunEnvBuilder({"BUILDKITE_AGENT_ID": ""}).worker_id_tag()

    assert tag == {}
