"""Sample test file run in a subprocess by the OTel integration tests.

Covers pass, fail, setup error, skip and xfail, plus a test that records its
own child span and a test that annotates itself.
"""

import time

import pytest
from opentelemetry import trace


@pytest.fixture
def broken_fixture():
    raise RuntimeError("fixture exploded")


def test_passing():
    time.sleep(0.01)


def test_failing():
    assert 1 == 2, "one is not two"


@pytest.mark.skip(reason="not today")
def test_skipped():
    assert False


@pytest.mark.xfail(reason="known bug")
def test_expected_failure():
    raise ValueError("expected")


def test_setup_error(broken_fixture):
    assert True


@pytest.mark.execution_tag("team", "backend")
@pytest.mark.execution_tag("ci.worker.id", "per-test")
def test_annotated(spans):
    with spans.measure("annotation", {"content": "hello from the test"}):
        pass


def test_with_child_span():
    with trace.get_tracer("sample").start_as_current_span("db.query"):
        time.sleep(0.01)
