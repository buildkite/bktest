"""pytest hooks that turn each test into a ``test.execution`` span.

``export`` and ``spans`` need the optional SDK, so they are imported in
``pytest_sessionstart``: an import failure falls back to the JSON upload, and
configuring there runs after conftest code has installed any suite-wide
tracer provider.
"""

import time
from contextvars import Token
from types import ModuleType
from typing import TYPE_CHECKING, Any, Callable, Dict, Generator, Mapping, Optional

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
    def _span_filter(pytest_config: pytest.Config) -> Optional[Callable[[Any], object]]:
        hook = getattr(pytest_config.hook, SPAN_FILTER_HOOK)
        if not hook.get_hookimpls():
            return None

        def predicate(span: Any) -> object:
            result = hook(span=span)
            return True if result is None else result
        return predicate

    @pytest.hookimpl(tryfirst=True)
    def pytest_unconfigure(self, config: pytest.Config) -> None:
        """pytest_unconfigure hook callback to push every finished span before pytest exits"""
        if self._state is None:
            return
        # pytest has printed its summary by now; flushing it first keeps the
        # export warnings on their own lines after it instead of glued onto
        # the still-open progress line (pytest_sessionfinish runs too early).
        try:
            reporter = config.pluginmanager.get_plugin("terminalreporter")
            if reporter is not None:
                reporter.flush()
        except Exception as error:  # pylint: disable=broad-except
            warn_failure("Could not flush pytest's terminal output", error)
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
        """pytest_runtest_protocol hook wrapper to restore the context once the test is reported"""
        yield
        if self._state is None:
            return
        self._spans.detach(self._tokens.pop(item.nodeid, None))
        span = self._test_spans.pop(item.nodeid, None)
        if span is not None:
            self._finish(span, self._collector.in_flight.get(item.nodeid), time.time_ns())

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
        if nodeid in self._test_spans:
            span = self._test_spans.pop(nodeid)
            end_time_ns: Optional[int] = time.time_ns()
        else:
            # Collection errors and other tests that never entered the runtest
            # protocol get a span timed at report time.
            span = self._spans.start_test_span(self._state, test)
            end_time_ns = None
        if span is not None:
            self._finish(span, test, end_time_ns)

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
