"""pytest hooks a test suite can implement to influence OTel mode."""

from typing import Any, Optional

import pytest


@pytest.hookspec(firstresult=True)
def pytest_buildkite_otel_span_filter(span: Any) -> Optional[bool]:  # pylint: disable=unused-argument
    """Decide whether a finished child span is uploaded alongside its test.

    ``span`` is a ``ReadableSpan``. Return ``False`` to drop it, ``True`` or
    ``None`` to keep it. Only spans recorded during a test on the global tracer
    provider reach this hook; ``test.execution`` spans never do.
    """
