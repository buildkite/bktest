"""OpenTelemetry (OTLP) export mode for the Buildkite pytest collector.

Only ``export`` and ``spans`` import the OpenTelemetry SDK (the optional
``otel`` extra); ``plugin`` imports them lazily in ``pytest_sessionstart`` so
the collector loads without the SDK installed.
"""

from ..logger import logger


def warn_failure(what: str, error: BaseException) -> None:
    """Log ``what`` followed by the exception's class and message."""
    logger.warning("%s: %s: %s", what, type(error).__name__, error)
