"""Starting, describing and finishing ``test.execution`` and phase spans.

Imports the OpenTelemetry API, so it is only imported once OTel mode is on.
"""

import traceback
import unittest
from contextvars import Token
from typing import Any, Optional, Tuple, Type

import pytest
from opentelemetry import context, trace
from opentelemetry.trace import Span, SpanKind, Status, StatusCode

from ...collector.payload import TestData
from . import warn_failure
from . import attributes as attrs
from .export import EXECUTION_KEY, EXECUTION_SPAN_KEY, ExportState, record_start_failure

TEST_SPAN_NAME = "test.execution"
ANNOTATION_EVENT = "test.annotation"
ANNOTATION_ATTRIBUTE = "buildkite.annotation"

ExcInfo = Tuple[Type[BaseException], BaseException, Any]

_SKIP_EXCEPTIONS: Tuple[Type[BaseException], ...] = (
    pytest.skip.Exception, pytest.xfail.Exception, unittest.SkipTest,
)


def start_test_span(state: ExportState, test: TestData) -> Optional[Span]:
    """Start the root ``test.execution`` span for ``test``; ``None`` (and a warning) on failure."""
    try:
        span_attrs: attrs.Attributes = {
            attrs.EXECUTION_VIA_ATTRIBUTE: attrs.EXECUTION_VIA,
            attrs.RUN_KEY_ATTRIBUTE: state.run_key,
            attrs.RESULT_ATTRIBUTE: attrs.RESULT_UNSET,
        }
        span_attrs.update(attrs.test_attributes(test))
        return state.test_tracer.start_span(
            TEST_SPAN_NAME,
            context=context.Context(),
            kind=SpanKind.INTERNAL,
            attributes=span_attrs,
            links=state.job_links,
        )
    except Exception as error:  # pylint: disable=broad-except
        record_start_failure(error)
        return None



def attach_test_span(span: Span) -> Optional[Token]:
    """Make ``span`` current and record its trace id for the child-span forwarder."""
    try:
        ctx = trace.set_span_in_context(span)
        ctx = context.set_value(EXECUTION_KEY, span.get_span_context().trace_id, ctx)
        ctx = context.set_value(EXECUTION_SPAN_KEY, span, ctx)
        return context.attach(ctx)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not make OpenTelemetry span current", error)
        return None


def detach(token: Optional[Token]) -> None:
    """Restore the context that was current before ``attach_test_span``."""
    if token is None:
        return
    try:
        context.detach(token)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not restore the OpenTelemetry context", error)


def finish_test_span(
    span: Span,
    test: TestData,
    run_attributes: attrs.Attributes,
    run_tags: attrs.Attributes,
    end_time_ns: Optional[int],
) -> None:
    """Record the result, re-describe the span and end it; each step fails independently."""
    try:
        _record_result(span, test)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not record the OpenTelemetry test result", error)
    try:
        _describe_test(span, test, run_attributes, run_tags)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not describe OpenTelemetry test span", error)
    try:
        # The wall clock can step backwards between start and finalize; the SDK
        # would then record a negative duration.
        start_time = getattr(span, "start_time", None)
        if end_time_ns is not None and start_time is not None and end_time_ns < start_time:
            end_time_ns = None
        span.end(end_time=end_time_ns)
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not finish OpenTelemetry test span", error)


def _record_result(span: Span, test: TestData) -> None:
    status = attrs.result_status(test)
    if status is None:
        return
    span.set_attribute(attrs.RESULT_ATTRIBUTE, status)
    if status != "fail":
        return
    span.set_status(Status(StatusCode.ERROR, attrs.failure_reason(test)))
    for event in attrs.exception_events(test):
        span.add_event("exception", event)


def _describe_test(
    span: Span, test: TestData, run_attributes: attrs.Attributes, run_tags: attrs.Attributes
) -> None:
    test_attributes = attrs.test_attributes(test)
    for key, value in test_attributes.items():
        span.set_attribute(key, value)
    for key, value in run_attributes.items():
        span.set_attribute(key, value)
    tags = dict(run_tags)
    tags.update(attrs.test_tag_attributes(test))
    for key, value in tags.items():
        span.set_attribute(key, value)
    # The Python SDK evicts the *oldest* attribute at the count limit, so the
    # keys the server needs to accept the execution are written last.
    span.set_attribute(attrs.EXECUTION_VIA_ATTRIBUTE, attrs.EXECUTION_VIA)
    span.set_attribute(attrs.RUN_KEY_ATTRIBUTE, run_attributes[attrs.RUN_KEY_ATTRIBUTE])
    span.set_attribute(attrs.RESULT_ATTRIBUTE, attrs.result_status(test) or attrs.RESULT_UNSET)
    span.set_attribute(attrs.SCOPE_ATTRIBUTE, test_attributes[attrs.SCOPE_ATTRIBUTE])
    span.set_attribute(attrs.NAME_ATTRIBUTE, test_attributes[attrs.NAME_ATTRIBUTE])


def current_test_span() -> Optional[Span]:
    """Return the recording test span of the running test, if any."""
    try:
        span = context.get_value(EXECUTION_SPAN_KEY)
        if isinstance(span, Span) and span.is_recording():
            return span
        return None
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not read the current OpenTelemetry test span", error)
        return None


def annotate(content: str) -> None:
    """Add a ``test.annotation`` event to the running test's span (no-op outside a test)."""
    try:
        span = current_test_span()
        if span is not None:
            span.add_event(ANNOTATION_EVENT, {ANNOTATION_ATTRIBUTE: str(content)})
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not annotate OpenTelemetry test span", error)


def start_phase_span(
    state: ExportState, test_span: Span, name: str
) -> Tuple[Optional[Span], Optional[Token]]:
    """Start a phase span under ``test_span`` and make it current (none without child export)."""
    try:
        tracer = state.phase_tracer
        if tracer is None:
            return None, None
        span = tracer.start_span(
            name, context=trace.set_span_in_context(test_span), kind=SpanKind.INTERNAL
        )
        token = context.attach(trace.set_span_in_context(span))
        return span, token
    except Exception as error:  # pylint: disable=broad-except
        warn_failure(f"Could not start OpenTelemetry {name} span", error)
        return None, None


def finish_phase_span(
    span: Optional[Span], token: Optional[Token], excinfo: Optional[ExcInfo]
) -> None:
    """Detach the phase, record any failure that escaped it, and end it."""
    if span is None:
        return
    detach(token)
    try:
        if excinfo is not None and not isinstance(excinfo[1], _SKIP_EXCEPTIONS):
            _record_phase_failure(span, excinfo)
        span.end()
    except Exception as error:  # pylint: disable=broad-except
        warn_failure("Could not finish OpenTelemetry phase span", error)


def _record_phase_failure(span: Span, excinfo: ExcInfo) -> None:
    exc_type, error, tb = excinfo
    message = attrs.scrub_utf8(str(error))
    span.set_status(Status(StatusCode.ERROR, message[: attrs.FAILURE_REASON_MAX_LENGTH]))
    stacktrace = attrs.scrub_utf8("".join(traceback.format_exception(exc_type, error, tb)))
    span.record_exception(error, attributes={
        "exception.message": attrs.truncate(message, attrs.EXCEPTION_MESSAGE_MAX_LENGTH),
        "exception.stacktrace": stacktrace[: attrs.EXCEPTION_STACKTRACE_MAX_LENGTH],
    })
