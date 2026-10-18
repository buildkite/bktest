"""Sample test file whose results are only known once pytest has finished reporting.

Run in a subprocess by the integration tests: a passing body with a failing
fixture finalizer, and (under ``--reruns 1``) a test that fails first and
passes on its second attempt, next to a plain pass and a skip.
"""

import os
from pathlib import Path

import pytest


@pytest.fixture
def broken_cleanup():
    yield
    raise RuntimeError("cleanup failed after passing body")


def test_passing_body_with_broken_cleanup(broken_cleanup):
    assert True


def test_flaky():
    marker = Path(os.environ["FLAKY_MARKER"])
    if not marker.exists():
        marker.write_text("first attempt failed")
        raise AssertionError("first attempt fails")


def test_plain_pass():
    assert True


@pytest.mark.skip(reason="not today")
def test_plain_skip():
    assert False
