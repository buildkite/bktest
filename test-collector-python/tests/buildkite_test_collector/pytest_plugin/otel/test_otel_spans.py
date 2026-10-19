"""Starting, describing and finishing test and phase spans."""

import subprocess
import sys
from dataclasses import replace

import pytest
from opentelemetry import context, trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from buildkite_test_collector.collector.payload import TestResultFailed
from buildkite_test_collector.pytest_plugin.otel import attributes as attrs
from buildkite_test_collector.pytest_plugin.otel import export, spans

VALID_TRACEPARENT = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"


@pytest.fixture
def state(monkeypatch):
    """An export state whose test provider (also installed globally, for phases) records in memory."""
    exporter = InMemorySpanExporter()
    provider = TracerProvider(span_limits=export.TEST_SPAN_LIMITS)
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(trace, "_TRACER_PROVIDER", provider)
    forwarder = export.ChildSpanForwarder(SimpleSpanProcessor(InMemorySpanExporter()), None)
    yield export.ExportState(
        run_key="run-1", test_provider=provider, test_processor=None, test_session=None,
        forwarder=forwarder,
    ), exporter
    provider.shutdown()


def test_test_span_is_a_root_even_when_another_span_is_current(state, successful_test):
    export_state, _ = state
    with trace.get_tracer("suite").start_as_current_span("outer"):
        span = spans.start_test_span(export_state, successful_test)
    span.end()
    assert span.parent is None


def test_links_to_the_job_span_only_with_a_valid_traceparent():
    links = export.job_span_links({"TRACEPARENT": VALID_TRACEPARENT, "TRACESTATE": "a=b"})
    assert len(links) == 1
    assert links[0].context.trace_id == 0x0AF7651916CD43DD8448EB211C80319C
    assert links[0].context.span_id == 0xB7AD6B7169203331
    assert export.job_span_links({"TRACEPARENT": "not-a-traceparent"}) == ()
    assert export.job_span_links({"TRACEPARENT": ""}) == ()
    assert export.job_span_links({}) == ()


def test_test_span_is_a_root_with_synthesis_attributes_first(state, successful_test):
    export_state, exporter = state
    linked = replace(export_state, job_links=export.job_span_links({"TRACEPARENT": VALID_TRACEPARENT}))
    span = spans.start_test_span(linked, successful_test)
    assert span.parent is None
    assert list(span.attributes)[:3] == [
        "buildkite.execution.via", "buildkite.run_key", "test.case.result.status"
    ]
    assert span.attributes["test.case.result.status"] == "unset"
    assert span.attributes["buildkite.run_key"] == "run-1"
    assert len(span.links) == 1
    spans.finish_test_span(span, successful_test, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()[0]
    assert finished.attributes["test.case.result.status"] == "pass"
    assert finished.status.status_code is trace.StatusCode.UNSET


def test_failure_becomes_status_and_exception_events(state, failed_test):
    export_state, exporter = state
    span = spans.start_test_span(export_state, failed_test)
    spans.finish_test_span(span, failed_test, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()[0]
    assert finished.attributes["test.case.result.status"] == "fail"
    assert finished.status.status_code is trace.StatusCode.ERROR
    assert finished.status.description == "bogus"
    assert [event.name for event in finished.events] == ["exception"]
    assert finished.events[0].attributes == {
        "exception.message": "test failed", "exception.stacktrace": "test.py:1",
    }


def test_identity_survives_129_attributes(state, successful_test):
    export_state, exporter = state
    noisy = replace(successful_test, tags={f"tag{i}": "v" for i in range(129)})
    span = spans.start_test_span(export_state, noisy)
    spans.finish_test_span(span, noisy, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()[0]
    assert len(finished.attributes) == 128
    assert finished.dropped_attributes > 0
    assert finished.attributes["buildkite.execution.via"] == "otlp"
    assert finished.attributes["buildkite.run_key"] == "run-1"
    assert finished.attributes["test.case.result.status"] == "pass"
    assert finished.attributes["buildkite.test.scope"] == "wyld stallyns"
    assert finished.attributes["buildkite.test.name"] == "san dimas meltdown"


def test_exception_events_keep_the_newest_100(state, successful_test):
    export_state, exporter = state
    many = replace(successful_test, result=TestResultFailed(
        "r", [{"expanded": [f"failure {i}"]} for i in range(105)]
    ))
    span = spans.start_test_span(export_state, many)
    spans.finish_test_span(span, many, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()[0]
    assert len(finished.events) == 100
    assert finished.events[-1].attributes["exception.message"] == "failure 104"
    assert finished.events[0].attributes["exception.message"] == "failure 5"


def test_per_test_tags_win_over_run_tags(state, successful_test):
    export_state, exporter = state
    tagged = replace(successful_test, tags={"team": "mine"})
    span = spans.start_test_span(export_state, tagged)
    spans.finish_test_span(
        span, tagged, {"buildkite.run_key": "run-1"},
        {"buildkite.tag.team": "theirs", "buildkite.tag.ci.worker.id": "w"},
    )
    finished = exporter.get_finished_spans()[0]
    assert finished.attributes["buildkite.tag.team"] == "mine"
    assert finished.attributes["buildkite.tag.ci.worker.id"] == "w"


def test_a_clock_that_steps_back_gives_a_zero_duration_not_a_negative_one(
    state, successful_test, monkeypatch
):
    export_state, exporter = state
    span = spans.start_test_span(export_state, successful_test)
    monkeypatch.setattr(spans, "wall_clock_ns", lambda: span.start_time - 1_000_000)
    spans.finish_test_span(span, successful_test, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()[0]
    assert finished.end_time == finished.start_time


def test_span_is_finished_even_when_it_cannot_be_described(state, successful_test, monkeypatch, capfd):
    export_state, exporter = state
    span = spans.start_test_span(export_state, successful_test)

    def broken(test):
        raise RuntimeError("describe broke")
    monkeypatch.setattr(attrs, "test_attributes", broken)
    spans.finish_test_span(span, successful_test, {"buildkite.run_key": "run-1"}, {})
    finished = exporter.get_finished_spans()
    assert len(finished) == 1
    assert finished[0].attributes["test.case.result.status"] == "pass"
    assert "Could not describe OpenTelemetry test span: RuntimeError: describe broke" \
        in capfd.readouterr().err


def test_start_failure_fails_open_and_reports_a_missing_result(state, successful_test, capfd):
    export_state, _ = state
    assert spans.start_test_span(export_state, replace(successful_test, name=None)) is None
    err = capfd.readouterr().err
    assert "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span" in err


def test_annotation_goes_on_the_test_span_not_the_current_span(state, successful_test):
    export_state, exporter = state
    spans.annotate("outside a test")
    test_span = spans.start_test_span(export_state, successful_test)
    token = spans.attach_test_span(test_span)
    try:
        phase, phase_token = spans.start_phase_span(export_state, test_span, "test.body")
        assert trace.get_current_span() is phase
        spans.annotate("inside")
        spans.finish_phase_span(phase, phase_token, None)
        assert trace.get_current_span() is test_span
    finally:
        spans.detach(token)
    test_span.end()
    assert trace.get_current_span() is trace.INVALID_SPAN
    by_name = {span.name: span for span in exporter.get_finished_spans()}
    assert [event.name for event in by_name["test.execution"].events] == ["test.annotation"]
    assert by_name["test.execution"].events[0].attributes == {"buildkite.annotation": "inside"}
    assert by_name["test.body"].events == ()
    assert by_name["test.body"].parent.span_id == test_span.get_span_context().span_id


def test_phase_records_failures_but_not_skips(state, successful_test):
    export_state, exporter = state
    test_span = spans.start_test_span(export_state, successful_test)

    phase, token = spans.start_phase_span(export_state, test_span, "test.setup")
    try:
        pytest.skip("skipped")
    except BaseException as error:  # pylint: disable=broad-except
        spans.finish_phase_span(phase, token, (type(error), error, None))

    phase, token = spans.start_phase_span(export_state, test_span, "test.body")
    error = AssertionError("x" * 20000)
    spans.finish_phase_span(phase, token, (AssertionError, error, None))

    by_name = {span.name: span for span in exporter.get_finished_spans()}
    assert by_name["test.setup"].status.status_code is trace.StatusCode.UNSET
    assert by_name["test.setup"].events == ()
    assert by_name["test.body"].status.status_code is trace.StatusCode.ERROR
    assert len(by_name["test.body"].status.description) == 1024
    event = by_name["test.body"].events[0]
    assert event.name == "exception"
    assert event.attributes["exception.type"] == "AssertionError"
    assert len(event.attributes["exception.message"]) == attrs.EXCEPTION_MESSAGE_MAX_LENGTH
    assert event.attributes["exception.message"].endswith("...")
    assert event.attributes["exception.stacktrace"].startswith("AssertionError: xxx")
    assert context.get_value(export.EXECUTION_SPAN_KEY) is None


def test_phase_spans_need_the_child_path(state, successful_test):
    export_state, _ = state
    test_span = spans.start_test_span(export_state, successful_test)
    without_children = replace(export_state, forwarder=None)
    assert spans.start_phase_span(without_children, test_span, "test.body") == (None, None)


def test_the_sdk_is_not_imported_by_loading_the_plugin():
    code = (
        "import sys, buildkite_test_collector.pytest_plugin as p;"
        "from buildkite_test_collector.pytest_plugin.otel import plugin;"
        "assert not [m for m in sys.modules if m.startswith('opentelemetry')], "
        "[m for m in sys.modules if m.startswith('opentelemetry')]"
    )
    subprocess.run([sys.executable, "-c", code], check=True)
