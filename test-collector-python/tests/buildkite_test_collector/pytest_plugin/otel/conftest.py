"""Fixtures for OTel mode tests."""

from typing import Iterator

import pytest

from otel_harness import OTLPReceiver


@pytest.fixture
def receiver() -> Iterator[OTLPReceiver]:
    """A running loopback OTLP receiver."""
    server = OTLPReceiver()
    server.start()
    yield server
    server.stop()
