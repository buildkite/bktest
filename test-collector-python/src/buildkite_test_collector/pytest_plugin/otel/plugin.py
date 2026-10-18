"""pytest hooks that turn each test into a ``test.execution`` span.

``export`` and ``spans`` need the optional SDK, so they are imported in
``pytest_sessionstart``: an import failure falls back to the JSON upload, and
configuring there runs after conftest code has installed any suite-wide
tracer provider.
"""

import time
from contextvars import Token
from types import ModuleType
from typing import TYPE_CHECKING, Any, Callable, Dict, Generator, List, Mapping, Optional, Tuple

import pytest

from ...collector.payload import TestData
from ...collector.run_env import RunEnv
from ..buildkite_plugin import BuildkitePlugin
from ..logger import logger
from . import warn_failure
from . import attributes as attrs
from . import config as otel_config

if TYPE_CHECKING:
    from opentelemetry.trace import Span

    from .export import ExportState

SPAN_FILTER_HOOK = "pytest_buildkite_otel_span_filter"
# Key under which an xdist worker sends its upload status to the controller
# in ``config.workeroutput``.
WORKER_STATUS_KEY = "buildkite_test_collector_otel"
# Controller-side record of (worker id, missing span count, reasons).
WORKER_LOSSES = pytest.StashKey[List[Tuple[str, int, str]]]()


class OTelPlugin:
    """Emit one root span per test alongside the JSON collector, replacing its upload."""

    # pylint: disable=too-many-instance-attributes

    def __init__(
        self,
        env: Mapping[str, Optional[str]],
        run_env: RunEnv,
        tags: Mapping[str, str],
        collector: BuildkitePlugin,
        otel: otel_config.OTelConfig,
    ):  # pylint: disable=too-many-arguments,too-many-positional-arguments
        self._env = env
        self._run_env = run_env
        self._run_attributes = attrs.run_attributes(env, run_env)
        self._run_tags = attrs.tag_attributes(tags)
        self._collector = collector
        self._otel = otel
        self._export: Optional[ModuleType] = None
        self._spans: Optional[ModuleType] = None
        self._state: Optional["ExportState"] = None
        self._test_spans: Dict[str, Optional["Span"]] = {}
        self._tokens: Dict[str, Token] = {}

    def pytest_sessionstart(self, session: pytest.Session) -> None:
        """pytest_sessionstart hook callback to configure export or fall back to JSON"""
        fallback = otel_config.json_fallback_outcome(self._env)
        if not otel_config.run_key_is_valid(self._run_env.key):
            logger.warning(
                "OpenTelemetry span export disabled: run key %r is invalid. "
                "Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); "
                "it must be 1-255 printable ASCII characters without spaces.", self._run_env.key,
            )
            logger.warning(
                "%s is set, but OpenTelemetry could not be configured (see the warning above); %s",
                otel_config.ENV_ENABLED, fallback,
            )
            return
        try:
            # pylint: disable=import-outside-toplevel
            from . import export, spans
            self._state = export.configure(
                self._env, self._run_env, self._otel, self._span_filter(session.config)
            )
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("OpenTelemetry span export disabled", error)
            logger.warning(
                "%s is set, but OpenTelemetry could not be configured (see the warning above); %s",
                otel_config.ENV_ENABLED, fallback,
            )
            return
        self._export = export
        self._spans = spans
        # Export may already belong to an earlier run in this process; the
        # span's run key must then match the request header, not this run.
        self._run_attributes[attrs.RUN_KEY_ATTRIBUTE] = self._state.run_key
        self._collector.otel = self

    @staticmethod
    def _span_filter(pytest_config: pytest.Config) -> Callable[[Any], object]:
        # Resolve the hook on every call: conftests found during collection
        # register their implementations after the session has started.
        hook = getattr(pytest_config.hook, SPAN_FILTER_HOOK)

        def predicate(span: Any) -> object:
            result = hook(span=span)
            return True if result is None else result
        return predicate

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(self, session: pytest.Session) -> None:
        """pytest_sessionfinish hook callback to push every finished span from an xdist worker"""
        # The xdist controller tears a worker down (SIGKILL after 10 seconds)
        # as soon as the worker's own sessionfinish wrapper reports it done,
        # which is before pytest_unconfigure and atexit and well inside the
        # flush budget; the worker's TEST RESULTS MISSING warning would die
        # with it. Flushing here, inside that wrapper but after pytest's own
        # session teardown, makes the controller wait for it. What could not
        # be uploaded travels to the controller with the worker's output, so
        # the controller can repeat it even when the worker's stderr did not
        # reach the log.
        if self._state is None or not _is_xdist_worker(session.config):
            return
        self._export.force_flush()
        try:
            missing, reasons = self._export.missing_test_spans()
            session.config.workeroutput[WORKER_STATUS_KEY] = {  # type: ignore[attr-defined]
                "missing": missing, "reasons": reasons,
            }
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not report OpenTelemetry upload status to the xdist controller",
                         error)

    @pytest.hookimpl(tryfirst=True)
    def pytest_unconfigure(self, config: pytest.Config) -> None:
        """pytest_unconfigure hook callback to push every finished span before pytest exits"""
        if self._state is None or _is_xdist_worker(config):
            return
        _flush_terminal(config)
        self._export.force_flush()

    @pytest.hookimpl(trylast=True)
    def pytest_runtest_logstart(self, nodeid: str) -> None:
        """pytest_runtest_logstart hook callback to start the test span from the JSON test data"""
        if self._state is None:
            return
        test = self._collector.in_flight.get(nodeid)
        if test is None:
            return
        # A failed start is recorded too (as None), so the test is not
        # reported a second time when its result arrives.
        span = self._spans.start_test_span(self._state, test)
        self._test_spans[nodeid] = span
        if span is not None:
            self._tokens[nodeid] = self._spans.attach_test_span(span)

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_protocol(self, item: pytest.Item) -> Generator[None, Any, None]:
        """pytest_runtest_protocol hook wrapper to report a test the protocol did not finish"""
        yield
        if self._state is None:
            return
        self._spans.detach(self._tokens.pop(item.nodeid, None))
        span = self._test_spans.pop(item.nodeid, None)
        if span is None:
            return
        # Only an interrupted test (Ctrl-C) is still here: pytest never
        # reported it, so it is exported as skipped, as the Playwright
        # collector does.
        test = self._collector.in_flight.get(item.nodeid)
        if test is not None and test.result is None:
            test = test.skipped()
        self._finish(span, test, time.time_ns())

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_setup(self, item: pytest.Item) -> Generator[None, Any, None]:
        """pytest_runtest_setup hook wrapper for the ``test.setup`` phase span"""
        yield from self._phase(item, "test.setup")

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_call(self, item: pytest.Item) -> Generator[None, Any, None]:
        """pytest_runtest_call hook wrapper for the ``test.body`` phase span"""
        yield from self._phase(item, "test.body")

    @pytest.hookimpl(hookwrapper=True, tryfirst=True)
    def pytest_runtest_teardown(self, item: pytest.Item) -> Generator[None, Any, None]:
        """pytest_runtest_teardown hook wrapper for the ``test.teardown`` phase span"""
        yield from self._phase(item, "test.teardown")

    def _phase(self, item: pytest.Item, name: str) -> Generator[None, Any, None]:
        span, token = None, None
        test_span = self._test_spans.get(item.nodeid)
        if test_span is not None:
            span, token = self._spans.start_phase_span(self._state, test_span, name)
        outcome = yield
        if span is not None:
            self._spans.finish_phase_span(span, token, outcome.excinfo)

    def finalized(self, nodeid: str, test: TestData) -> None:
        """Finish the test's span now that the JSON collector has settled its result."""
        if nodeid not in self._test_spans:
            self.report(test)
            return
        # Detach here too: pytest-rerunfailures starts the next attempt (a new
        # span) inside the same runtest protocol.
        self._spans.detach(self._tokens.pop(nodeid, None))
        span = self._test_spans.pop(nodeid)
        if span is not None:
            self._finish(span, test, time.time_ns())

    def report(self, test: TestData) -> None:
        """Send a span, timed now, for a test that never entered the runtest protocol."""
        span = self._spans.start_test_span(self._state, test)
        if span is not None:
            self._finish(span, test, None)

    def _finish(self, span: "Span", test: TestData, end_time_ns: Optional[int]) -> None:
        try:
            self._spans.finish_test_span(
                span, test, self._run_attributes, self._run_tags, end_time_ns
            )
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not finish the OpenTelemetry test span", error)

    def annotate(self, content: str) -> None:
        """Add a ``test.annotation`` event to the running test's span."""
        if self._state is not None:
            self._spans.annotate(content)


def _is_xdist_worker(config: pytest.Config) -> bool:
    return hasattr(config, "workerinput")


def _flush_terminal(config: pytest.Config) -> None:
    # pytest has printed its summary by now; flushing it first keeps the
    # export warnings on their own lines after it instead of glued onto
    # the still-open progress line.
    try:
        reporter = config.pluginmanager.get_plugin("terminalreporter")
        if reporter is not None:
            reporter.flush()
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not flush pytest's terminal output", error)


def record_worker_status(node: Any, error: object) -> None:
    """Keep what an xdist worker reported it could not upload, for the controller's summary."""
    # xdist reports a worker that finished normally once with ``error=None``;
    # a crashed or interrupted worker is reported (again) with an error, and
    # a crashed one never sent its status at all.
    if error is not None:
        return
    status = (getattr(node, "workeroutput", None) or {}).get(WORKER_STATUS_KEY)
    if status and status["missing"]:
        losses = node.config.stash.setdefault(WORKER_LOSSES, [])
        losses.append((node.gateway.id, status["missing"], status["reasons"]))


def report_worker_losses(config: pytest.Config) -> None:
    """Warn, from the xdist controller, about every test span a worker could not upload."""
    losses = config.stash.get(WORKER_LOSSES, [])
    if not losses:
        return
    _flush_terminal(config)
    for worker, missing, reasons in losses:
        logger.warning(
            "TEST RESULTS MISSING: OpenTelemetry on xdist worker %s did not upload %d "
            "test.execution span(s) (%s); those test executions were not uploaded to "
            "Buildkite Test Engine.", worker, missing, reasons,
        )
    del config.stash[WORKER_LOSSES]


def build(
    env: Mapping[str, Optional[str]],
    run_env: RunEnv,
    tags: Mapping[str, str],
    collector: BuildkitePlugin,
) -> Optional[OTelPlugin]:
    """Return a plugin when OTel mode is switched on and has a credential, else ``None``."""
    otel = otel_config.OTelConfig.from_env(env)
    if not otel.requested():
        return None
    return OTelPlugin(env, run_env, tags, collector, otel)
