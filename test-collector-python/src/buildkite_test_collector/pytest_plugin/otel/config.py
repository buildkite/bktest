"""OTel mode configuration read from environment variables (no SDK imports)."""

import re
from dataclasses import dataclass
from typing import Mapping, Optional, Tuple

from ..logger import logger

ENV_ENABLED = "BUILDKITE_ANALYTICS_OTEL_ENABLED"
ENV_OTLP_TOKEN = "BUILDKITE_TESTS_OTLP_TOKEN"
ENV_ANALYTICS_TOKEN = "BUILDKITE_ANALYTICS_TOKEN"
ENV_ENDPOINT = "BUILDKITE_ANALYTICS_OTLP_ENDPOINT"

DEFAULT_ENDPOINT = "https://tests-otlp.buildkite.com/v1/traces"

RUN_KEY_FORMAT = re.compile(r"\A[!-~]{1,255}\Z")

SPAN_PROCESSOR_SIZE_LIMIT = 2**31 - 1

TEST_SPAN_QUEUE_SIZE_ENV = "BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE"
TEST_SPAN_BATCH_SIZE_ENV = "BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE"
CHILD_SPAN_QUEUE_SIZE_ENV = "BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE"
CHILD_SPAN_BATCH_SIZE_ENV = "BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE"

TEST_SPAN_DEFAULT_SIZES = (8192, 120)
CHILD_SPAN_DEFAULT_SIZES = (2048, 512)


@dataclass(frozen=True)
class OTelConfig:
    """What the environment asked OTel mode to do."""

    enabled: bool
    token: Optional[str]
    endpoint: str

    @classmethod
    def from_env(cls, env: Mapping[str, Optional[str]]) -> "OTelConfig":
        """Read the OTel switch and, only when it is on, the credential and endpoint."""
        enabled = (env.get(ENV_ENABLED) or "").strip().lower() in ("1", "true")
        if not enabled:
            return cls(enabled=False, token=None, endpoint=DEFAULT_ENDPOINT)
        token = _stripped(env.get(ENV_OTLP_TOKEN)) or _stripped(env.get(ENV_ANALYTICS_TOKEN))
        endpoint = env.get(ENV_ENDPOINT)
        if endpoint is None:
            endpoint = DEFAULT_ENDPOINT
        return cls(enabled=enabled, token=token, endpoint=endpoint)

    def requested(self) -> bool:
        """Return whether OTel mode is both switched on and has a credential to send."""
        return self.enabled and self.token is not None


def json_fallback_outcome(env: Mapping[str, Optional[str]]) -> str:
    """Describe what happens to results when OTel mode falls back to the JSON path."""
    if _stripped(env.get(ENV_ANALYTICS_TOKEN)):
        return "uploading results as JSON instead"
    return f"results will not be uploaded because {ENV_ANALYTICS_TOKEN} is not set"


def run_key_is_valid(run_key: str) -> bool:
    """Return whether ``run_key`` is acceptable as the ``Buildkite-Tests-Run-Key`` header."""
    return RUN_KEY_FORMAT.match(run_key) is not None


def span_processor_sizes(
    env: Mapping[str, Optional[str]],
    queue_env: str,
    batch_env: str,
    defaults: Tuple[int, int],
) -> Tuple[int, int]:
    """Resolve ``(queue_size, batch_size)`` from env overrides, warning on invalid values."""
    default_queue, default_batch = defaults
    queue = _positive_int(env.get(queue_env), queue_env, default_queue)
    batch = _positive_int(env.get(batch_env), batch_env, default_batch)
    if batch > queue:
        logger.warning(
            "%s must be <= %s; using defaults (batch %d, queue %d)",
            batch_env, queue_env, default_batch, default_queue,
        )
        return defaults
    return queue, batch


def _positive_int(raw: Optional[str], name: str, default: int) -> int:
    if raw is None:
        return default
    if re.fullmatch(r"[0-9]+", raw):
        try:
            value = int(raw)
        except ValueError:  # Python 3.11+ rejects int() of more than ~4300 digits
            value = 0
        if 1 <= value <= SPAN_PROCESSOR_SIZE_LIMIT:
            return value
    logger.warning(
        "%s must be a positive integer up to %d, using default %d",
        name, SPAN_PROCESSOR_SIZE_LIMIT, default,
    )
    return default


def _stripped(value: Optional[str]) -> Optional[str]:
    if value is None:
        return None
    value = value.strip()
    return value or None
