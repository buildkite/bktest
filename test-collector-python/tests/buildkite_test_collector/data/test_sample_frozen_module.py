"""Tests run while a module-scoped fixture freezes time."""

import pytest
from freezegun import freeze_time


@pytest.fixture(scope="module", autouse=True)
def frozen():
    with freeze_time("2001-01-01"):
        yield


def test_module_first():
    pass


def test_module_second():
    pass


def test_module_third():
    pass
