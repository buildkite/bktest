"""Span attribute builders shared by the test span and the producer resource.

Built from the same ``RunEnv``/``TestData`` the JSON path uploads (plus env
and runtime versions), so test identity cannot drift between the two paths.
"""

import platform
import re
from typing import Dict, List, Mapping, Optional, Tuple, Union

import pytest

from ...collector.constants import COLLECTOR_NAME, TEST_RUNNER, VERSION
from ...collector.payload import (
    TestData,
    TestResultFailed,
    TestResultPassed,
    TestResultSkipped,
)
from ...collector.run_env import RunEnv

AttributeValue = Union[str, int]
Attributes = Dict[str, AttributeValue]

EXECUTION_VIA_ATTRIBUTE = "buildkite.execution.via"
EXECUTION_VIA = "otlp"
RUN_KEY_ATTRIBUTE = "buildkite.run_key"
RESULT_ATTRIBUTE = "test.case.result.status"
RESULT_UNSET = "unset"
SCOPE_ATTRIBUTE = "buildkite.test.scope"
NAME_ATTRIBUTE = "buildkite.test.name"
TAG_ATTRIBUTE_PREFIX = "buildkite.tag."

FAILURE_REASON_MAX_LENGTH = 1024
# 10_240 plus the "..." the SDK appends when it truncates; matches TEST_SPAN_LIMITS.
EXCEPTION_MESSAGE_MAX_LENGTH = 10243
EXCEPTION_STACKTRACE_MAX_LENGTH = 16384

_LONE_SURROGATE = re.compile("[\ud800-\udfff]")


def producer_resource_attributes(
    env: Mapping[str, Optional[str]], run_env: RunEnv
) -> Attributes:
    """Build the attributes identifying the producer (suite, pipeline run, worker, VCS ref)."""
    run_id, run_url = ci_pipeline_run(env, run_env)
    attrs: Dict[str, Optional[str]] = {
        "service.name": env.get("BUILDKITE_TEST_ENGINE_SUITE_SLUG"),
        "service.namespace": env.get("BUILDKITE_ORGANIZATION_SLUG"),
        "cicd.pipeline.run.id": run_id,
        "cicd.pipeline.run.url.full": run_url,
        "cicd.worker.id": _non_blank(env.get("BUILDKITE_AGENT_ID")),
        "process.runtime.version": platform.python_version(),
        "vcs.ref.head.name": run_env.branch,
        "vcs.ref.head.revision": run_env.commit_sha,
    }
    if run_env.branch is not None:
        attrs["vcs.ref.type"] = "tag" if env.get("BUILDKITE_TAG") else "branch"
    return _compact(attrs)


def ci_pipeline_run(
    env: Mapping[str, Optional[str]], run_env: RunEnv
) -> Tuple[Optional[str], Optional[str]]:
    """Return the CI provider's native run id and URL (``None`` outside a known CI)."""
    if run_env.ci == "buildkite":
        run_id = _non_empty(env.get("BUILDKITE_BUILD_ID"))
        url = _non_empty(env.get("BUILDKITE_BUILD_URL"))
    elif run_env.ci == "github_actions":
        run_id = _non_empty(env.get("GITHUB_RUN_ID"))
        repo = _non_empty(env.get("GITHUB_REPOSITORY"))
        url = f"https://github.com/{repo}/actions/runs/{run_id}" if repo and run_id else None
    elif run_env.ci == "circleci":
        run_id = _non_empty(env.get("CIRCLE_WORKFLOW_ID"))
        url = None
    else:
        return None, None
    if run_id is None:
        return None, None
    return run_id, url


def run_attributes(env: Mapping[str, Optional[str]], run_env: RunEnv) -> Attributes:
    """Run metadata carried on every test span (excluding tags)."""
    _, native_url = ci_pipeline_run(env, run_env)
    attrs: Dict[str, Optional[str]] = {
        RUN_KEY_ATTRIBUTE: run_env.key,
        "buildkite.build_number": run_env.number,
        "buildkite.job_id": run_env.job_id,
        "buildkite.message": run_env.message,
        "buildkite.step_id": env.get("BUILDKITE_STEP_ID"),
        "buildkite.collector.name": COLLECTOR_NAME,
        "buildkite.collector.version": VERSION,
        "buildkite.test.framework.name": TEST_RUNNER,
        "buildkite.test.framework.version": pytest.__version__,
    }
    if run_env.url is not None and run_env.url != native_url:
        attrs["buildkite.run_url"] = run_env.url
    return _compact(attrs)


def tag_attributes(tags: Mapping[str, str]) -> Attributes:
    """Tags as ``buildkite.tag.<key>`` string attributes."""
    return {f"{TAG_ATTRIBUTE_PREFIX}{key}": str(value) for key, value in tags.items()}


def test_attributes(test: TestData) -> Attributes:
    """Build the identity and location attributes from the ``TestData`` the JSON path uploads."""
    attrs: Attributes = {
        SCOPE_ATTRIBUTE: scrub_utf8(test.scope),
        NAME_ATTRIBUTE: scrub_utf8(test.name),
        "test.case.name": scrub_utf8(test_case_name(test)),
        "test.suite.name": scrub_utf8(test.scope),
        "buildkite.test.execution.external_id": str(test.id),
    }
    if test.file_name is not None:
        attrs["code.file.path"] = scrub_utf8(test.file_name)
    line = line_number(test)
    if line is not None:
        attrs["code.line.number"] = line
    return attrs


def test_tag_attributes(test: TestData) -> Attributes:
    """Build the per-test tag (``execution_tag`` marker) attributes."""
    return {
        f"{TAG_ATTRIBUTE_PREFIX}{key}": scrub_utf8(value) for key, value in test.tags.items()
    }


def test_case_name(test: TestData) -> str:
    """Return the test as pytest renders it (``scope::name``)."""
    return f"{test.scope}::{test.name}" if test.scope else test.name


def line_number(test: TestData) -> Optional[int]:
    """Return the line number from a ``file:line`` JSON location, if present."""
    if test.location is None or test.location == test.file_name:
        return None
    _, _, line = test.location.rpartition(":")
    return int(line) if line.isdigit() else None


def result_status(test: TestData) -> Optional[str]:
    """Map the JSON result to ``pass``/``fail``/``skipped`` (``None`` when unset)."""
    if isinstance(test.result, TestResultPassed):
        return "pass"
    if isinstance(test.result, TestResultFailed):
        return "fail"
    if isinstance(test.result, TestResultSkipped):
        return "skipped"
    return None


def failure_reason(test: TestData) -> Optional[str]:
    """Return the span status description for a failed test, capped at 1,024 characters."""
    if not isinstance(test.result, TestResultFailed) or not test.result.failure_reason:
        return None
    return scrub_utf8(test.result.failure_reason)[:FAILURE_REASON_MAX_LENGTH]


def exception_events(test: TestData) -> List[Dict[str, str]]:
    """Build one ``exception`` event's attributes per ``failure_expanded`` entry."""
    if not isinstance(test.result, TestResultFailed) or not test.result.failure_expanded:
        return []
    events = []
    for failure in test.result.failure_expanded:
        attrs: Dict[str, str] = {}
        message = "\n".join(failure.get("expanded") or ())
        if message:
            attrs["exception.message"] = truncate(scrub_utf8(message), EXCEPTION_MESSAGE_MAX_LENGTH)
        backtrace = "\n".join(failure.get("backtrace") or ())
        if backtrace:
            attrs["exception.stacktrace"] = scrub_utf8(backtrace)[:EXCEPTION_STACKTRACE_MAX_LENGTH]
        if attrs:
            events.append(attrs)
    return events


def truncate(value: str, limit: int) -> str:
    """Cut ``value`` to ``limit`` characters, marking the cut with ``...``."""
    if len(value) <= limit:
        return value
    return value[: limit - 3] + "..."


def scrub_utf8(value: str) -> str:
    """Replace lone surrogates (which protobuf cannot encode) with U+FFFD."""
    return _LONE_SURROGATE.sub("\ufffd", value)


def _compact(attrs: Mapping[str, Optional[str]]) -> Attributes:
    return {key: value for key, value in attrs.items() if value is not None}


def _non_empty(value: Optional[str]) -> Optional[str]:
    return value or None


def _non_blank(value: Optional[str]) -> Optional[str]:
    return value if value is not None and value.strip() else None
