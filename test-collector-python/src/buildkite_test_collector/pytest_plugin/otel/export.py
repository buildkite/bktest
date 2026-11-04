"""The OTLP export pipeline: providers, processors, exporters and drop accounting.

This module imports the OpenTelemetry SDK, so it must only be imported once a
session has decided to use OTel mode.
"""

import atexit
import logging
import os
import re
import secrets
import threading
import time
from contextlib import contextmanager
from dataclasses import dataclass, field, replace
from functools import cached_property
from importlib.metadata import version
from typing import Any, Callable, Dict, Iterator, List, Mapping, Optional, Sequence, Set, Tuple
from urllib.parse import urlsplit

import requests
from opentelemetry import context, trace
from opentelemetry.exporter.otlp.proto.http import Compression
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import ReadableSpan, Span, SpanLimits, SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter, SpanExportResult
from opentelemetry.sdk.trace.id_generator import IdGenerator
from opentelemetry.sdk.trace.sampling import ALWAYS_ON
from opentelemetry.trace import Link
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from ...collector.constants import VERSION
from ...collector.run_env import RunEnv
from ..logger import logger
from . import warn_failure
from . import config
from .attributes import producer_resource_attributes

TRACER_NAME = "buildkite-test-collector"
RUN_KEY_HEADER = "Buildkite-Tests-Run-Key"
SPAN_STREAM_HEADER = "Buildkite-Tests-Span-Stream"

# Older exporters retried 408 and every 5xx (including 500) with no
# Retry-After support; Buildkite must never see a 500 retried.
EXPORTER_DISTRIBUTION = "opentelemetry-exporter-otlp-proto-http"
MINIMUM_EXPORTER_VERSION = (1, 45)

# Environment the SDK would otherwise merge into the Buildkite stream.
OTLP_EXPORTER_ENV_PREFIXES = ("OTEL_EXPORTER_OTLP_", "OTEL_PYTHON_EXPORTER_OTLP_")
SDK_DISABLED_ENV = "OTEL_SDK_DISABLED"

PROCESSOR_TIMEOUT_SECONDS = 30
EXPORT_TIMEOUT_MILLISECONDS = 30_000
TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS = 1_000
CHILD_SPAN_SCHEDULE_DELAY_MILLISECONDS = 5_000

# Every limit is given explicitly: any left unset is read from OTEL_SPAN_* /
# OTEL_ATTRIBUTE_* in the environment, which must not shape the test span.
TEST_SPAN_LIMITS = SpanLimits(
    max_attributes=128,
    max_span_attributes=128,
    # 10_240 plus the 3-char "..." the SDK appends when truncating; the server
    # strips the suffix and truncates to 10_240 before deriving the test id.
    max_span_attribute_length=10_243,
    max_events=100,
    max_event_attributes=128,
    max_links=128,
    max_link_attributes=128,
    max_attribute_length=16_384,
)

PHASE_SPAN_NAMES = frozenset(("test.setup", "test.body", "test.teardown"))

# Context key carrying the running test's trace id, so ChildSpanForwarder.on_start
# can tell that test's descendants from unrelated spans.
EXECUTION_KEY = context.create_key("buildkite.test.execution")
# Context key carrying the test span itself (while setup, body or teardown runs the
# current span is that phase span, not the test span).
EXECUTION_SPAN_KEY = context.create_key("buildkite.test.execution.span")

SpanFilterCallable = Callable[[ReadableSpan], object]


class SecureRandomIdGenerator(IdGenerator):
    """Trace and span ids from the OS CSPRNG, unaffected by seeded PRNGs in tests."""

    def generate_span_id(self) -> int:
        """Generate a non-zero 64-bit span id."""
        while True:
            span_id = secrets.randbits(64)
            if span_id:
                return span_id

    def generate_trace_id(self) -> int:
        """Generate a non-zero 128-bit trace id."""
        while True:
            trace_id = secrets.randbits(128)
            if trace_id:
                return trace_id

    def is_trace_id_random(self) -> bool:
        """Report random trace ids so the SDK sets the W3C random-trace-id flag."""
        return True


def job_span_links(env: Mapping[str, Optional[str]]) -> Tuple[Link, ...]:
    """Return a link to the Buildkite Agent's job span when ``TRACEPARENT`` is a valid context."""
    carrier = {
        key: env[source]
        for key, source in (("traceparent", "TRACEPARENT"), ("tracestate", "TRACESTATE"))
        if env.get(source)
    }
    if not carrier:
        return ()
    span_context = trace.get_current_span(
        TraceContextTextMapPropagator().extract(carrier)
    ).get_span_context()
    return (Link(span_context),) if span_context.is_valid else ()


class RecordingSession(requests.Session):
    """A requests session that stamps the current credential and remembers the last failure."""

    def __init__(self, authorization: str):
        super().__init__()
        self.authorization = authorization
        self.last_failure: Optional[str] = None

    def send(self, request: requests.PreparedRequest, **kwargs: Any) -> requests.Response:
        """Send ``request`` with the current ``Authorization`` header, recording the outcome."""
        request.headers["Authorization"] = self.authorization
        try:
            response = super().send(request, **kwargs)
        except Exception as error:
            self.last_failure = type(error).__name__
            raise
        self.last_failure = None if 200 <= response.status_code < 300 else str(response.status_code)
        return response


@dataclass
class StreamAccounting:  # pylint: disable=too-many-instance-attributes
    """Counts of what happened to one stream's spans; the SDK offers no drop callback."""

    offered: int = 0
    handed: int = 0
    succeeded: int = 0
    failed: int = 0
    start_failures: int = 0
    last_failure: Optional[str] = None
    reported: int = 0
    # offered - handed as seen when the queue was last known to be empty:
    # spans the queue turned away, as opposed to spans still waiting in it.
    queue_losses: int = 0

    def pending(self) -> int:
        """Count spans whose export has neither succeeded nor failed yet."""
        return self.offered - self.succeeded - self.failed - self.queue_losses

    def dropped(self) -> int:
        """Count spans that will never reach Buildkite."""
        return self.start_failures + self.failed + self.queue_losses


_warned_export_errors: Set[str] = set()


class CountingExporter(SpanExporter):
    """Wrap the OTLP exporter so no exception reaches the batch worker and every span is counted."""

    def __init__(
        self, inner: SpanExporter, accounting: StreamAccounting, session: RecordingSession
    ):
        self._inner = inner
        self._accounting = accounting
        self._session = session

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        """Export ``spans``, converting exceptions into a counted failure."""
        self._accounting.handed += len(spans)
        self._session.last_failure = None
        try:
            result = self._inner.export(spans)
        except Exception as error:  # pylint: disable=broad-except
            name = type(error).__name__
            if name not in _warned_export_errors:
                _warned_export_errors.add(name)
                logger.warning(
                    "Could not export OpenTelemetry spans: %s. "
                    "Further %s export failures will not be reported.", name, name,
                )
            self._accounting.failed += len(spans)
            self._accounting.last_failure = name
            return SpanExportResult.FAILURE
        # The exporter counts a 3xx as success although it does not follow
        # redirects; the session saw the real status.
        if result is SpanExportResult.SUCCESS and self._session.last_failure is None:
            self._accounting.succeeded += len(spans)
            self._accounting.last_failure = None
            return result
        self._accounting.failed += len(spans)
        self._accounting.last_failure = self._session.last_failure or "unknown"
        return SpanExportResult.FAILURE

    def shutdown(self) -> None:
        """Shut down the wrapped exporter."""
        self._inner.shutdown()


class CountingBatchSpanProcessor(BatchSpanProcessor):
    """A batch processor that counts every span offered to it."""

    def __init__(self, exporter: SpanExporter, accounting: StreamAccounting, **kwargs: Any):
        super().__init__(exporter, **kwargs)
        self._accounting = accounting

    def on_end(self, span: ReadableSpan) -> None:
        """Count the span, then queue it."""
        self._accounting.offered += 1
        super().on_end(span)


class SpanFilter:  # pylint: disable=too-few-public-methods
    """Run the customer's child-span filter, retaining the span whenever the filter misbehaves."""

    def __init__(self, predicate: SpanFilterCallable):
        self._predicate = predicate
        self._warned = False
        self._local = threading.local()

    def accept(self, span: ReadableSpan) -> bool:
        """Return whether ``span`` should be exported."""
        # A predicate that ends spans re-enters on_end on this thread; let those through.
        if getattr(self._local, "running", False):
            return True
        self._local.running = True
        try:
            return bool(self._predicate(span))
        except Exception as error:  # pylint: disable=broad-except
            if not self._warned:
                self._warned = True
                logger.warning(
                    "Could not filter OpenTelemetry child span, retaining it: %s: %s. "
                    "Further filter failures will not be reported.", type(error).__name__, error,
                )
            return True
        finally:
            self._local.running = False


@dataclass
class _Tracked:  # pylint: disable=too-few-public-methods
    phase: bool
    populated: bool = False


class ChildSpanForwarder(SpanProcessor):
    """Route spans from a test's trace to the child stream; everything else is ignored."""

    def __init__(self, processor: SpanProcessor, span_filter: Optional[SpanFilter]):
        self._processor = processor
        self._filter = span_filter
        self._lock = threading.Lock()
        self._active = True
        self._tracked: Dict[int, _Tracked] = {}

    def on_start(self, span: Span, parent_context: Optional[context.Context] = None) -> None:
        """Remember spans that belong to the current test's trace."""
        try:
            trace_id = context.get_value(EXECUTION_KEY, parent_context)
            if trace_id is None or span.context.trace_id != trace_id:
                return
            parent_id = trace.get_current_span(parent_context).get_span_context().span_id
            with self._lock:
                if not self._active:
                    return
                phase = span.name in PHASE_SPAN_NAMES
                self._tracked[span.context.span_id] = _Tracked(phase=phase)
                parent = self._tracked.get(parent_id)
                if not phase and parent is not None and parent.phase:
                    parent.populated = True
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not track OpenTelemetry child span", error)

    def on_end(self, span: ReadableSpan) -> None:
        """Hand a finished tracked span to the child processor if it should be exported."""
        try:
            with self._lock:
                tracked = self._tracked.pop(span.context.span_id, None)
                if tracked is None or not self._active:
                    return
                if tracked.phase:
                    # Failures already ride on the test span; only populated
                    # phases reach the child stream, bypassing the filter.
                    if tracked.populated:
                        self._processor.on_end(span)
                    return
                if self._filter is None:
                    self._processor.on_end(span)
                    return
            if not self._filter.accept(span):
                return
            with self._lock:
                if self._active:
                    self._processor.on_end(span)
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not export OpenTelemetry child span", error)

    def shutdown(self) -> None:
        """Stop forwarding; the child processor is shut down by the collector, not here."""
        with self._lock:
            self._active = False
            self._tracked.clear()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        """Flush the child processor while active."""
        with self._lock:
            active = self._active
        if not active:
            return True
        try:
            return self._processor.force_flush(timeout_millis)
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not flush OpenTelemetry child spans", error)
            return False


@dataclass(frozen=True)
class ExportState:
    """Everything configured for one run; one per process until shutdown."""

    # pylint: disable=too-many-instance-attributes
    run_key: str
    test_provider: TracerProvider
    test_processor: CountingBatchSpanProcessor
    test_session: RecordingSession
    job_links: Tuple[Link, ...] = ()
    test_accounting: StreamAccounting = field(default_factory=StreamAccounting)
    child_processor: Optional[CountingBatchSpanProcessor] = None
    child_session: Optional[RecordingSession] = None
    child_accounting: StreamAccounting = field(default_factory=StreamAccounting)
    forwarder: Optional[ChildSpanForwarder] = None

    # Both tracers are resolved once per state: providers do not change after
    # configuration, and a lookup per span measurably slows large suites.
    @cached_property
    def test_tracer(self) -> trace.Tracer:
        """Return the tracer test spans are started from."""
        return self.test_provider.get_tracer(TRACER_NAME, VERSION)

    @cached_property
    def phase_tracer(self) -> Optional[trace.Tracer]:
        """Return the global-provider tracer for phase spans, or ``None`` without child export."""
        if self.forwarder is None:
            return None
        return trace.get_tracer(TRACER_NAME, VERSION)


_state: Optional[ExportState] = None
_exit_registered = False  # pylint: disable=invalid-name


def current() -> Optional[ExportState]:
    """Return the configured export state, if any."""
    return _state


def configure(
    env: Mapping[str, Optional[str]],
    run_env: RunEnv,
    otel: config.OTelConfig,
    span_filter: Optional[SpanFilterCallable],
) -> ExportState:
    """Set up export for ``run_env``; raise when the mandatory test-span path cannot be built."""
    global _state, _exit_registered  # pylint: disable=global-statement
    _check_exporter_version()
    _check_endpoint(otel.endpoint)
    authorization = f'Token token="{otel.token}"'
    if _state is not None:
        if _state.run_key != run_env.key:
            logger.warning(
                "OpenTelemetry span export is already configured for run %r and cannot switch to "
                "run %r: results will still be attributed to the earlier run. Reporting a new run "
                "requires a new process.", _state.run_key, run_env.key,
            )
        _refresh_authorization(_state, authorization)
        return _state

    resource = Resource.create(producer_resource_attributes(env, run_env))
    state = _build_test_stream(env, run_env.key, otel.endpoint, authorization, resource)
    state = replace(state, job_links=job_span_links(env))
    try:
        state = _build_child_stream(state, env, otel.endpoint, authorization, resource, span_filter)
    except Exception as error:  # pylint: disable=broad-except
        logger.warning(
            "OpenTelemetry child span export disabled: %s: %s; "
            "test.execution export remains enabled", type(error).__name__, error,
        )
    _state = state
    if not _exit_registered:
        _exit_registered = True
        atexit.register(shutdown)
    logger.debug(
        "OpenTelemetry span export configured: run key %r, endpoint %s, child spans %s",
        run_env.key, otel.endpoint, "on" if state.forwarder is not None else "off",
    )
    return state


def _check_exporter_version() -> None:
    found = version(EXPORTER_DISTRIBUTION)
    if _release_tuple(found) < MINIMUM_EXPORTER_VERSION:
        minimum = ".".join(str(part) for part in MINIMUM_EXPORTER_VERSION)
        raise ImportError(
            f"{EXPORTER_DISTRIBUTION} {found} is installed but {minimum} or newer is required"
        )


def _release_tuple(version_string: str) -> Tuple[int, ...]:
    release = re.match(r"\d+(?:\.\d+)*", version_string)
    return tuple(int(part) for part in release.group().split(".")) if release else ()


def _check_endpoint(endpoint: str) -> None:
    parts = urlsplit(endpoint)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        raise ValueError(
            f"{config.ENV_ENDPOINT} must be an http(s) URL, got {endpoint!r}"
        )


def _build_test_stream(
    env: Mapping[str, Optional[str]],
    run_key: str,
    endpoint: str,
    authorization: str,
    resource: Resource,
) -> ExportState:
    queue_size, batch_size = config.span_processor_sizes(
        env, config.TEST_SPAN_QUEUE_SIZE_ENV, config.TEST_SPAN_BATCH_SIZE_ENV,
        config.TEST_SPAN_DEFAULT_SIZES,
    )
    accounting = StreamAccounting()
    session = RecordingSession(authorization)
    exporter = CountingExporter(
        _otlp_exporter(endpoint, authorization, run_key, "test", session), accounting, session
    )
    processor = CountingBatchSpanProcessor(
        exporter, accounting,
        max_queue_size=queue_size, max_export_batch_size=batch_size,
        schedule_delay_millis=TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS,
        export_timeout_millis=EXPORT_TIMEOUT_MILLISECONDS,
    )
    try:
        # OTEL_SDK_DISABLED=true makes the provider hand out no-op tracers:
        # nothing would be queued, nothing dropped, and the run would be
        # silently empty. Test spans are not an opt-in signal.
        with _without_env(SDK_DISABLED_ENV):
            provider = TracerProvider(
                sampler=ALWAYS_ON, resource=resource, id_generator=SecureRandomIdGenerator(),
                span_limits=TEST_SPAN_LIMITS, shutdown_on_exit=False,
            )
        provider.add_span_processor(processor)
    except Exception:
        processor.shutdown()
        raise
    return ExportState(
        run_key=run_key, test_provider=provider, test_processor=processor,
        test_session=session, test_accounting=accounting,
    )


def _build_child_stream(
    state: ExportState,
    env: Mapping[str, Optional[str]],
    endpoint: str,
    authorization: str,
    resource: Resource,
    span_filter: Optional[SpanFilterCallable],
) -> ExportState:
    # pylint: disable=too-many-arguments,too-many-positional-arguments
    queue_size, batch_size = config.span_processor_sizes(
        env, config.CHILD_SPAN_QUEUE_SIZE_ENV, config.CHILD_SPAN_BATCH_SIZE_ENV,
        config.CHILD_SPAN_DEFAULT_SIZES,
    )
    session = RecordingSession(authorization)
    exporter = CountingExporter(
        _otlp_exporter(endpoint, authorization, state.run_key, "child", session),
        state.child_accounting, session,
    )
    processor = CountingBatchSpanProcessor(
        exporter, state.child_accounting,
        max_queue_size=queue_size, max_export_batch_size=batch_size,
        schedule_delay_millis=CHILD_SPAN_SCHEDULE_DELAY_MILLISECONDS,
        export_timeout_millis=EXPORT_TIMEOUT_MILLISECONDS,
    )
    forwarder = ChildSpanForwarder(processor, SpanFilter(span_filter) if span_filter else None)
    try:
        _attach_to_global_provider(forwarder, resource)
    except Exception:
        forwarder.shutdown()
        processor.shutdown()
        raise
    return replace(state, child_processor=processor, child_session=session, forwarder=forwarder)


class ProviderRefusalWatch(logging.Filter):  # pylint: disable=too-few-public-methods
    """Notice the API refusing a suite's tracer provider because the collector's is installed.

    The API only logs a warning, which pytest's log capture hides, and a
    logging filter cannot log itself, so the collector warns later.
    """

    REFUSAL = "Overriding of current TracerProvider is not allowed"

    def __init__(self) -> None:
        super().__init__()
        self.refused = False

    def filter(self, record: logging.LogRecord) -> bool:
        """Note the refusal and pass the record on."""
        if record.getMessage() == self.REFUSAL:
            self.refused = True
        return True


_provider_refusal = ProviderRefusalWatch()


def warn_if_provider_refused() -> None:
    """Warn, once, if a suite's own tracer provider was refused because ours was installed."""
    if not _provider_refusal.refused:
        return
    _provider_refusal.refused = False
    logger.warning(
        "OpenTelemetry: trace.set_tracer_provider() was called after the collector installed "
        "its own tracer provider at session start, so that provider was not installed and its "
        "exporters receive no spans (spans inside a test still reach Buildkite as child spans). "
        "Set your tracer provider when a conftest.py that pytest loads at startup is imported "
        "(in the rootdir or a test path given on the command line), in a plugin, or before "
        "pytest starts; a fixture or a conftest.py found during collection is too late."
    )


def _attach_to_global_provider(forwarder: ChildSpanForwarder, resource: Resource) -> None:
    provider = trace.get_tracer_provider()
    if isinstance(provider, trace.ProxyTracerProvider):
        managed = TracerProvider(
            resource=resource, id_generator=SecureRandomIdGenerator(), shutdown_on_exit=False
        )
        managed.add_span_processor(forwarder)
        trace.set_tracer_provider(managed)
        if trace.get_tracer_provider() is not managed:
            raise RuntimeError("could not install the collector-managed tracer provider")
        logging.getLogger(trace.__name__).addFilter(_provider_refusal)
        return
    if not callable(getattr(provider, "add_span_processor", None)):
        raise TypeError(f"{type(provider).__name__} does not accept span processors")
    provider.add_span_processor(forwarder)


def _otlp_exporter(
    endpoint: str, authorization: str, run_key: str, stream: str, session: RecordingSession
) -> OTLPSpanExporter:
    headers = {
        "Authorization": authorization,
        RUN_KEY_HEADER: run_key,
        SPAN_STREAM_HEADER: stream,
    }
    with _without_env(*OTLP_EXPORTER_ENV_PREFIXES):
        return OTLPSpanExporter(
            endpoint=endpoint, headers=headers, timeout=PROCESSOR_TIMEOUT_SECONDS,
            compression=Compression.Gzip, session=session,
        )


@contextmanager
def _without_env(*prefixes: str) -> Iterator[None]:
    """Hide environment variables starting with ``prefixes`` while an SDK object is built.

    The exporter merges env headers into explicit ones and reads certificate
    variables whenever the explicit option is ``None``, and the tracer
    provider reads its kill switch; scrubbing the process environment for the
    duration of the constructor is the only supported way to keep the
    Buildkite stream to exactly its own settings.
    """
    hidden = {key: os.environ.pop(key) for key in list(os.environ) if key.startswith(prefixes)}
    try:
        yield
    finally:
        os.environ.update(hidden)


def _refresh_authorization(state: ExportState, authorization: str) -> None:
    state.test_session.authorization = authorization
    if state.child_session is not None:
        state.child_session.authorization = authorization


def force_flush() -> None:
    """Push every finished span to Buildkite within one shared 30 s budget."""
    state = _state
    if state is None:
        return
    deadline = time.monotonic() + PROCESSOR_TIMEOUT_SECONDS
    _drain("flush", "test", state.test_processor.force_flush, state.test_accounting, deadline,
           _report_test_drops)
    if state.forwarder is not None:
        _drain("flush", "child", state.forwarder.force_flush, state.child_accounting, deadline,
               _report_child_drops)


def shutdown() -> None:
    """Stop export once: forwarder inert first, then both processors against one shared budget."""
    global _state  # pylint: disable=global-statement
    state = _state
    if state is None:
        return
    _state = None
    deadline = time.monotonic() + PROCESSOR_TIMEOUT_SECONDS
    if state.forwarder is not None:
        try:
            state.forwarder.shutdown()
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not stop OpenTelemetry child span forwarding", error)
    _drain("shutdown", "test", state.test_processor.shutdown, state.test_accounting, deadline,
           _report_test_drops)
    if state.child_processor is not None:
        _drain(
            "shutdown", "child", state.child_processor.shutdown, state.child_accounting, deadline,
            _report_child_drops,
        )
    _warned_export_errors.clear()


def _drain(
    stage: str,
    stream: str,
    operation: Callable[[], object],
    accounting: StreamAccounting,
    deadline: float,
    report: Callable[[StreamAccounting], None],
) -> None:
    """Run one stream's ``operation`` until ``deadline``, then warn about what it left behind."""
    # pylint: disable=too-many-arguments,too-many-positional-arguments
    try:
        completed = _within_deadline(operation, deadline)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure(f"OpenTelemetry {stream} span {stage} failed", error)
        completed = False
    if completed:
        accounting.queue_losses = accounting.offered - accounting.handed
    logger.debug(
        "OpenTelemetry %s stream after %s (completed=%s): offered=%d handed=%d succeeded=%d "
        "failed=%d start_failures=%d queue_losses=%d last_failure=%s",
        stream, stage, completed, accounting.offered, accounting.handed, accounting.succeeded,
        accounting.failed, accounting.start_failures, accounting.queue_losses,
        accounting.last_failure,
    )
    report(accounting)


def _within_deadline(operation: Callable[[], object], deadline: float) -> bool:
    """Run ``operation`` on a daemon thread; return whether it completed before ``deadline``.

    An operation that returns ``False`` (a processor flush that timed out
    internally) did not complete. An exception it raises is re-raised here.
    """
    outcome: List[object] = []

    def run() -> None:
        try:
            outcome.append(operation())
        except BaseException as error:  # pylint: disable=broad-except
            outcome.append(error)

    thread = threading.Thread(target=run, name="buildkite-otel-drain", daemon=True)
    try:
        thread.start()
    except RuntimeError:
        # The process-exit shutdown runs in an exit handler, where Python may
        # refuse new threads (3.12.0-3.12.2 do). Run it here instead, as the
        # SDK's own exit handler does: the processor's shutdown timeout then
        # bounds it rather than the deadline.
        if time.monotonic() >= deadline:
            return False
        run()
    else:
        thread.join(max(0.0, deadline - time.monotonic()))
        if thread.is_alive():
            return False
    if isinstance(outcome[0], BaseException):
        raise outcome[0]
    return outcome[0] is not False


def record_start_failure(error: BaseException) -> None:
    """Count a test span that could not be started and say so immediately."""
    if _state is not None:
        _state.test_accounting.start_failures += 1
        _state.test_accounting.reported += 1
    logger.warning(
        "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) "
        "(could not start span: %s: %s). OpenTelemetry is the only upload path, so those test "
        "executions were not uploaded to Buildkite Test Engine.", type(error).__name__, error,
    )


def _report_test_drops(accounting: StreamAccounting) -> None:
    dropped = accounting.dropped()
    if dropped > accounting.reported:
        accounting.reported = dropped
        logger.warning(
            "TEST RESULTS MISSING: OpenTelemetry dropped %d test.execution span(s) so far this run "
            "(%s); those test executions were not uploaded to Buildkite Test Engine.",
            dropped, ", ".join(_drop_reasons(accounting)),
        )
    if accounting.pending() > 0:
        logger.warning(
            "TEST RESULTS MISSING: OpenTelemetry could not confirm export of %d test.execution "
            "span(s) within %d seconds (still pending%s); if the process exits now those test "
            "executions will not be uploaded to Buildkite Test Engine.",
            accounting.pending(), PROCESSOR_TIMEOUT_SECONDS, _last_failure_suffix(accounting),
        )


def _report_child_drops(accounting: StreamAccounting) -> None:
    dropped = accounting.dropped()
    if dropped > accounting.reported:
        accounting.reported = dropped
        logger.warning(
            "OpenTelemetry dropped %d child span(s) (%s); test.execution results are unaffected.",
            dropped, ", ".join(_drop_reasons(accounting)),
        )
    if accounting.pending() > 0:
        logger.warning(
            "OpenTelemetry could not confirm export of %d child span(s) within %d seconds%s; "
            "test.execution results are unaffected.",
            accounting.pending(), PROCESSOR_TIMEOUT_SECONDS, _last_failure_suffix(accounting),
        )


def missing_test_spans() -> Tuple[int, str]:
    """Return how many test spans this process has not uploaded, with the reasons."""
    if _state is None:
        return 0, ""
    accounting = _state.test_accounting
    reasons = _drop_reasons(accounting)
    pending = accounting.pending()
    if pending > 0:
        reasons.append(f"not confirmed within {PROCESSOR_TIMEOUT_SECONDS} seconds: {pending}")
    return accounting.dropped() + pending, ", ".join(reasons)


def _drop_reasons(accounting: StreamAccounting) -> List[str]:
    reasons = []
    if accounting.start_failures:
        reasons.append(f"could not start span: {accounting.start_failures}")
    if accounting.queue_losses:
        reasons.append(f"not-exported: {accounting.queue_losses}")
    if accounting.failed:
        reasons.append(f"export-failure: {accounting.failed}{_last_failure_suffix(accounting)}")
    return reasons


def _last_failure_suffix(accounting: StreamAccounting) -> str:
    if accounting.last_failure is None:
        return ""
    return f", last OTLP failure: {accounting.last_failure}"
