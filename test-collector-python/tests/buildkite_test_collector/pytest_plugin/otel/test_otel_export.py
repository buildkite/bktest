"""The export pipeline: child-span forwarding, lifecycle, reconfiguration and drop accounting."""

import random
import time
from dataclasses import replace
from types import SimpleNamespace
from typing import List

import pytest
from opentelemetry import context, trace
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SpanExporter, SpanExportResult
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF
from opentelemetry.trace import Status, StatusCode

from buildkite_test_collector.collector.run_env import RunEnvBuilder
from buildkite_test_collector.pytest_plugin.otel import config, export, plugin, spans


class RecordingProcessor:
    """A child processor that remembers what it was handed."""

    def __init__(self):
        self.ended: List[ReadableSpan] = []
        self.shutdowns = 0
        self.flushes = 0

    def on_start(self, span, parent_context=None):
        """Ignore starts."""

    def on_end(self, span):
        """Remember the span."""
        self.ended.append(span)

    def shutdown(self):
        """Count shutdowns."""
        self.shutdowns += 1

    def force_flush(self, timeout_millis=30000):
        """Count flushes."""
        self.flushes += 1
        return True


@pytest.fixture
def fresh_otel(monkeypatch):
    """Reset the collector's export state and the SDK's global provider around a test."""
    export.shutdown()
    monkeypatch.setattr(trace, "_TRACER_PROVIDER", None)
    monkeypatch.setattr(trace, "_TRACER_PROVIDER_SET_ONCE", type(trace._TRACER_PROVIDER_SET_ONCE)())
    monkeypatch.setattr(trace, "_PROXY_TRACER_PROVIDER", trace.ProxyTracerProvider())
    monkeypatch.setattr(export, "_exit_registered", False)
    yield
    export.shutdown()


@pytest.fixture
def run_env():
    return RunEnvBuilder({"BUILDKITE_BUILD_ID": "build-1"}).build()


def otel_config(receiver, token="tok"):
    return config.OTelConfig(enabled=True, token=token, endpoint=f"{receiver.url}/v1/traces")


def forwarder_with(span_filter=None):
    processor = RecordingProcessor()
    forwarder = export.ChildSpanForwarder(
        processor, export.SpanFilter(span_filter) if span_filter else None
    )
    provider = TracerProvider()
    provider.add_span_processor(forwarder)
    return provider.get_tracer("t"), forwarder, processor


def in_test_trace(tracer):
    """Start a root span and return a context that marks its trace as the execution trace."""
    root = tracer.start_span("test.execution")
    ctx = trace.set_span_in_context(root)
    ctx = context.set_value(export.EXECUTION_KEY, root.get_span_context().trace_id, ctx)
    return root, ctx


def test_forwarder_only_forwards_spans_from_the_execution_trace():
    tracer, _, processor = forwarder_with()
    root, ctx = in_test_trace(tracer)
    tracer.start_span("inside", context=ctx).end()
    tracer.start_span("outside").end()
    tracer.start_span("outside-with-context", context=trace.set_span_in_context(root)).end()
    root.end()
    assert [span.name for span in processor.ended] == ["inside"]


def test_forwarder_filter_decides_and_phases_bypass_it():
    tracer, _, processor = forwarder_with(lambda span: span.name == "keep")
    _, ctx = in_test_trace(tracer)
    tracer.start_span("keep", context=ctx).end()
    tracer.start_span("drop", context=ctx).end()
    failed_phase = tracer.start_span("test.setup", context=ctx)
    tracer.start_span("drop", context=trace.set_span_in_context(failed_phase, ctx)).end()
    failed_phase.set_status(Status(StatusCode.ERROR, "boom"))
    failed_phase.end()
    assert [span.name for span in processor.ended] == ["keep", "test.setup"]
    assert processor.ended[-1].status.status_code is StatusCode.ERROR
    assert processor.ended[-1].status.description == "boom"


def test_forwarder_exports_a_phase_iff_populated():
    tracer, _, processor = forwarder_with(lambda span: False)
    _, ctx = in_test_trace(tracer)

    empty_phase = tracer.start_span("test.body", context=ctx)
    empty_phase.end()

    failed_empty_phase = tracer.start_span("test.setup", context=ctx)
    failed_empty_phase.set_status(Status(StatusCode.ERROR, "boom"))
    failed_empty_phase.end()

    populated_phase = tracer.start_span("test.body", context=ctx)
    tracer.start_span("child", context=trace.set_span_in_context(populated_phase, ctx)).end()
    populated_phase.end()

    late_phase = tracer.start_span("test.teardown", context=ctx)
    late_phase.end()
    tracer.start_span("late child", context=trace.set_span_in_context(late_phase, ctx)).end()

    assert [span.name for span in processor.ended] == ["test.body"]
    assert processor.ended[0].context.span_id == populated_phase.get_span_context().span_id


def test_forwarder_retains_spans_when_the_filter_fails_and_warns_once(capfd):
    def broken(span):
        raise ValueError("filter bug")
    tracer, _, processor = forwarder_with(broken)
    _, ctx = in_test_trace(tracer)
    tracer.start_span("a", context=ctx).end()
    tracer.start_span("b", context=ctx).end()
    assert [span.name for span in processor.ended] == ["a", "b"]
    err = capfd.readouterr().err
    assert err.count("Could not filter OpenTelemetry child span, retaining it: ValueError: filter bug") == 1


def test_forwarder_retains_a_span_the_filter_finishes_itself():
    def ends_it(span):
        span.end()
        return False
    tracer, _, processor = forwarder_with(ends_it)
    _, ctx = in_test_trace(tracer)
    span = tracer.start_span("self-ending", context=ctx)
    span.end()
    assert [span.name for span in processor.ended] == ["self-ending"]


def test_forwarder_is_inert_after_shutdown_without_shutting_down_the_processor():
    tracer, forwarder, processor = forwarder_with()
    _, ctx = in_test_trace(tracer)
    before = tracer.start_span("before", context=ctx)
    forwarder.shutdown()
    before.end()
    tracer.start_span("after", context=ctx).end()
    assert processor.ended == []
    assert processor.shutdowns == 0
    assert forwarder.force_flush() is True
    assert processor.flushes == 0


def test_forwarder_never_raises_from_the_processor(capfd):
    class Exploding(RecordingProcessor):
        def on_end(self, span):
            raise RuntimeError("queue broke")
    processor = Exploding()
    forwarder = export.ChildSpanForwarder(processor, None)
    provider = TracerProvider()
    provider.add_span_processor(forwarder)
    tracer = provider.get_tracer("t")
    _, ctx = in_test_trace(tracer)
    tracer.start_span("child", context=ctx).end()
    assert "Could not export OpenTelemetry child span: RuntimeError: queue broke" \
        in capfd.readouterr().err


def test_configure_installs_a_managed_provider_and_routes_children(
    fresh_otel, run_env, receiver, successful_test
):
    env = {"BUILDKITE_BUILD_ID": "build-1"}
    state = export.configure(env, run_env, otel_config(receiver), None)
    assert export.current() is state
    assert isinstance(trace.get_tracer_provider(), TracerProvider)
    assert state.forwarder is not None

    test_span = spans.start_test_span(state, successful_test)
    token = spans.attach_test_span(test_span)
    try:
        trace.get_tracer("app").start_span("db.query").end()
    finally:
        spans.detach(token)
    test_span.end()
    export.force_flush()

    assert [span.name for span in receiver.spans("test")] == ["test.execution"]
    assert [span.name for span in receiver.spans("child")] == ["db.query"]
    assert receiver.spans("child")[0].trace_id == receiver.spans("test")[0].trace_id
    assert receiver.spans("test")[0].resource["cicd.pipeline.run.id"] == "build-1"


@pytest.mark.parametrize("endpoint", ["", "tests-otlp.buildkite.com/v1/traces", "ftp://relay/v1/traces", "http://"])
def test_configure_rejects_an_endpoint_that_is_not_an_http_url(fresh_otel, run_env, endpoint):
    otel = config.OTelConfig(enabled=True, token="tok", endpoint=endpoint)
    with pytest.raises(ValueError, match="BUILDKITE_ANALYTICS_OTLP_ENDPOINT must be an http"):
        export.configure({}, run_env, otel, None)
    assert export.current() is None


@pytest.mark.parametrize("found, accepted", [
    ("1.44.0", False), ("1.45.0", True), ("1.45", True), ("1.45.1", True), ("2.0.0b1", True), ("junk", False),
])
def test_configure_requires_an_exporter_that_never_retries_a_500(
    fresh_otel, run_env, receiver, monkeypatch, found, accepted
):
    monkeypatch.setattr(export, "version", lambda distribution: found)
    if accepted:
        export.configure({}, run_env, otel_config(receiver), None)
        assert export.current() is not None
    else:
        with pytest.raises(ImportError, match=f"{found} is installed but 1.45 or newer is required"):
            export.configure({}, run_env, otel_config(receiver), None)
        assert export.current() is None



def test_configure_attaches_to_a_suite_owned_provider(fresh_otel, run_env, receiver):
    suite_provider = TracerProvider()
    trace.set_tracer_provider(suite_provider)
    state = export.configure({}, run_env, otel_config(receiver), None)
    assert trace.get_tracer_provider() is suite_provider
    assert state.forwarder is not None


def test_configure_keeps_test_export_when_the_suite_provider_is_incompatible(
    fresh_otel, run_env, receiver, capfd, successful_test
):
    trace.set_tracer_provider(trace.NoOpTracerProvider())
    state = export.configure({}, run_env, otel_config(receiver), None)
    assert state.forwarder is None
    assert state.phase_tracer is None
    err = capfd.readouterr().err
    assert "OpenTelemetry child span export disabled: TypeError" in err
    assert "test.execution export remains enabled" in err

    spans.start_test_span(state, successful_test).end()
    export.force_flush()
    assert len(receiver.spans("test")) == 1


def test_test_spans_are_exported_when_the_suite_sampler_drops_children(
    fresh_otel, run_env, receiver, successful_test
):
    trace.set_tracer_provider(TracerProvider(sampler=ALWAYS_OFF))
    state = export.configure({}, run_env, otel_config(receiver), None)
    test_span = spans.start_test_span(state, successful_test)
    token = spans.attach_test_span(test_span)
    trace.get_tracer("app").start_span("dropped").end()
    spans.detach(token)
    test_span.end()
    export.force_flush()
    assert len(receiver.spans("test")) == 1
    assert receiver.spans("child") == []


def test_reconfigure_refreshes_the_token_and_keeps_the_run_key(
    fresh_otel, run_env, receiver, capfd, successful_test
):
    state = export.configure({}, run_env, otel_config(receiver, "first"), None)
    other_run = replace(run_env, key="build-2")
    again = export.configure({}, other_run, otel_config(receiver, "second"), None)
    assert again is state
    assert state.run_key == "build-1"
    err = capfd.readouterr().err
    assert "already configured for run 'build-1' and cannot switch to run 'build-2'" in err

    spans.start_test_span(state, successful_test).end()
    export.force_flush()
    request = receiver.trace_requests("test")[0]
    assert request.header("Authorization") == 'Token token="second"'
    assert request.header("Buildkite-Tests-Run-Key") == "build-1"


def test_span_ids_do_not_repeat_when_a_suite_seeds_random(fresh_otel, run_env, receiver):
    state = export.configure({}, run_env, otel_config(receiver), None)
    trace_ids = []
    for tracer in (state.test_tracer, state.test_tracer, state.phase_tracer, state.phase_tracer):
        random.seed(1234)
        span = tracer.start_span("seeded")
        trace_ids.append(span.get_span_context().trace_id)
        span.end()
    assert len(set(trace_ids)) == len(trace_ids)


def test_configure_after_shutdown_starts_fresh(fresh_otel, run_env, receiver, monkeypatch, capfd):
    registered = []
    monkeypatch.setattr(export.atexit, "register", registered.append)
    export.configure({}, run_env, otel_config(receiver), None)
    export.shutdown()
    second = export.configure({}, replace(run_env, key="build-2"), otel_config(receiver), None)
    assert second.run_key == "build-2"
    assert "cannot switch" not in capfd.readouterr().err
    assert registered == [export.shutdown]


def test_private_provider_failure_shuts_down_the_processor_and_raises(
    fresh_otel, run_env, receiver, monkeypatch
):
    shutdowns = []
    original = export.CountingBatchSpanProcessor.shutdown
    monkeypatch.setattr(
        export.CountingBatchSpanProcessor, "shutdown",
        lambda self: (shutdowns.append(self), original(self)),
    )

    def broken_provider(*args, **kwargs):
        raise RuntimeError("no provider")
    monkeypatch.setattr(export, "TracerProvider", broken_provider)
    with pytest.raises(RuntimeError, match="no provider"):
        export.configure({}, run_env, otel_config(receiver), None)
    assert len(shutdowns) == 1
    assert export.current() is None


def test_standard_otlp_env_is_hidden_only_while_building_exporters(
    fresh_otel, run_env, receiver, monkeypatch, successful_test
):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-env=1")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")
    state = export.configure({}, run_env, otel_config(receiver), None)
    assert export.os.environ["OTEL_EXPORTER_OTLP_HEADERS"] == "x-env=1"
    spans.start_test_span(state, successful_test).end()
    export.force_flush()
    request = receiver.trace_requests("test")[0]
    assert request.header("x-env") is None
    assert request.header("Buildkite-Tests-Span-Stream") == "test"


class FailingExporter(SpanExporter):
    """An inner exporter scripted per call: a result, an exception, or ``(result, http status)``."""

    def __init__(self, outcomes, session):
        self.outcomes = list(outcomes)
        self.session = session

    def export(self, batch):
        outcome = self.outcomes.pop(0) if self.outcomes else SpanExportResult.SUCCESS
        if isinstance(outcome, Exception):
            raise outcome
        if isinstance(outcome, tuple):
            outcome, self.session.last_failure = outcome
        return outcome

    def shutdown(self):
        """Nothing to release."""


def counting_pipeline(outcomes):
    accounting = export.StreamAccounting()
    session = export.RecordingSession("Token token=\"x\"")
    exporter = export.CountingExporter(FailingExporter(outcomes, session), accounting, session)
    return exporter, accounting


def test_exporter_names_each_exception_class_once_and_fails_the_batch(capfd):
    exporter, accounting = counting_pipeline([ValueError("a"), ValueError("b"), TypeError("c")])
    assert exporter.export([object()]) is SpanExportResult.FAILURE
    assert exporter.export([object(), object()]) is SpanExportResult.FAILURE
    assert exporter.export([object()]) is SpanExportResult.FAILURE
    assert accounting.failed == 4
    assert accounting.last_failure == "TypeError"
    err = capfd.readouterr().err
    assert err.count("Could not export OpenTelemetry spans: ValueError") == 1
    assert err.count("Could not export OpenTelemetry spans: TypeError") == 1


def test_exporter_records_the_http_status_and_forgets_it_after_success():
    exporter, accounting = counting_pipeline([(SpanExportResult.FAILURE, "503"), SpanExportResult.SUCCESS])
    exporter.export([object()])
    assert accounting.last_failure == "503"
    exporter.export([object()])
    assert accounting.last_failure is None
    assert accounting.failed == 1
    assert accounting.succeeded == 1


def test_exporter_counts_an_unfollowed_redirect_as_a_failure():
    exporter, accounting = counting_pipeline([(SpanExportResult.SUCCESS, "307")])
    assert exporter.export([object(), object()]) is SpanExportResult.FAILURE
    assert accounting.failed == 2
    assert accounting.succeeded == 0
    assert accounting.last_failure == "307"


def test_exporter_does_not_blame_an_earlier_status_for_a_failure_before_sending():
    exporter, accounting = counting_pipeline([(SpanExportResult.FAILURE, "503"), SpanExportResult.FAILURE])
    exporter.export([object()])
    exporter.export([object()])
    assert accounting.last_failure == "unknown"


def drained(accounting, stream="test", completed=True):
    """Report ``accounting`` as the end of a drain that did or did not complete."""
    report = export._report_test_drops if stream == "test" else export._report_child_drops
    export._drain("flush", stream, lambda: completed, accounting, time.monotonic() + 5, report)


def test_drop_report_names_the_cause_once_and_reports_later_drops(capfd):
    accounting = export.StreamAccounting(offered=5, handed=5, succeeded=3, failed=2, last_failure="400")
    drained(accounting)
    drained(accounting)
    err = capfd.readouterr().err
    assert err.count("TEST RESULTS MISSING") == 1
    assert "dropped 2 test.execution span(s) so far this run (export-failure: 2, last OTLP failure: 400)" in err

    accounting.offered += 3
    accounting.start_failures += 1
    drained(accounting)
    err = capfd.readouterr().err
    assert "dropped 6 test.execution span(s)" in err
    assert "could not start span: 1, not-exported: 3, export-failure: 2, last OTLP failure: 400" in err


def test_drop_report_does_not_blame_an_earlier_failure_for_a_full_buffer(capfd):
    accounting = export.StreamAccounting(offered=4, handed=2, succeeded=2, failed=0, last_failure=None)
    drained(accounting)
    err = capfd.readouterr().err
    assert "(not-exported: 2)" in err
    assert "last OTLP failure" not in err


def test_missing_test_spans_counts_drops_and_unconfirmed_spans(fresh_otel, run_env, receiver):
    assert export.missing_test_spans() == (0, "")
    state = export.configure({}, run_env, otel_config(receiver), None)
    state.test_accounting.offered = 6
    state.test_accounting.handed = 5
    state.test_accounting.succeeded = 2
    state.test_accounting.failed = 1
    state.test_accounting.last_failure = "ConnectionError"
    assert export.missing_test_spans() == (
        4, "export-failure: 1, last OTLP failure: ConnectionError, not confirmed within 30 seconds: 3",
    )


def test_drop_report_stays_silent_when_nothing_was_dropped(capfd):
    drained(export.StreamAccounting(offered=3, handed=3, succeeded=3))
    drained(export.StreamAccounting(offered=3, handed=3, succeeded=3), stream="child")
    assert capfd.readouterr().err == ""


def test_pending_spans_at_the_deadline_are_reported_as_unconfirmed(capfd):
    accounting = export.StreamAccounting(offered=4, handed=1, failed=0, last_failure="ConnectionError")
    drained(accounting, completed=False)
    err = capfd.readouterr().err
    assert "could not confirm export of 4 test.execution span(s) within 30 seconds" in err
    assert "still pending, last OTLP failure: ConnectionError" in err
    assert "dropped" not in err


def test_an_export_still_in_flight_is_pending_even_when_the_drain_completed(capfd):
    accounting = export.StreamAccounting(offered=7, handed=7, succeeded=2, failed=0)
    drained(accounting)
    err = capfd.readouterr().err
    assert "could not confirm export of 5 test.execution span(s)" in err
    assert "dropped" not in err

    accounting.failed += 5
    drained(accounting)
    err = capfd.readouterr().err
    assert "dropped 5 test.execution span(s) so far this run (export-failure: 5)" in err
    assert "could not confirm" not in err


def test_spans_the_queue_turned_away_are_not_pending_at_a_later_timeout(capfd):
    accounting = export.StreamAccounting(offered=5, handed=3, succeeded=3)
    drained(accounting)
    assert "dropped 2 test.execution span(s) so far this run (not-exported: 2)" in capfd.readouterr().err

    accounting.offered += 4
    accounting.handed += 1
    drained(accounting, completed=False)
    err = capfd.readouterr().err
    assert "could not confirm export of 4 test.execution span(s)" in err
    assert "dropped" not in err


def test_a_flush_that_times_out_inside_the_processor_did_not_complete(capfd):
    accounting = export.StreamAccounting(offered=2)
    drained(accounting, completed=False)
    assert "could not confirm export of 2 test.execution span(s)" in capfd.readouterr().err
    drained(accounting, completed=True)
    assert "dropped 2 test.execution span(s) so far this run (not-exported: 2)" in capfd.readouterr().err


def test_child_drops_are_reported_as_best_effort(capfd):
    accounting = export.StreamAccounting(offered=3, handed=3, failed=2, last_failure="500")
    drained(accounting, stream="child")
    err = capfd.readouterr().err
    assert "OpenTelemetry dropped 2 child span(s) (export-failure: 2, last OTLP failure: 500); " \
        "test.execution results are unaffected." in err
    assert "OpenTelemetry could not confirm export of 1 child span(s) within 30 seconds, " \
        "last OTLP failure: 500; test.execution results are unaffected." in err
    assert "TEST RESULTS MISSING" not in err


def test_start_failure_counts_as_a_dropped_result(
    fresh_otel, run_env, receiver, capfd, successful_test
):
    state = export.configure({}, run_env, otel_config(receiver), None)
    test_span = spans.start_test_span(state, replace(successful_test, name=None))
    assert test_span is None
    err = capfd.readouterr().err
    assert "TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (could not start span: " in err
    assert state.test_accounting.start_failures == 1
    export.force_flush()
    assert "TEST RESULTS MISSING" not in capfd.readouterr().err


def test_flush_and_shutdown_warn_instead_of_raising(fresh_otel, run_env, receiver, monkeypatch, capfd):
    state = export.configure({}, run_env, otel_config(receiver), None)

    def broken(timeout_millis=30000):
        raise RuntimeError("flush broke")
    monkeypatch.setattr(state.test_processor, "force_flush", broken)
    child_flushes = []
    monkeypatch.setattr(state.forwarder, "force_flush", lambda timeout_millis=30000: child_flushes.append(1) or True)
    export.force_flush()
    assert "OpenTelemetry test span flush failed: RuntimeError: flush broke" in capfd.readouterr().err
    assert child_flushes == [1]

    monkeypatch.setattr(state.test_processor, "shutdown", lambda: (_ for _ in ()).throw(RuntimeError("down")))
    export.shutdown()
    assert "OpenTelemetry test span shutdown failed: RuntimeError: down" in capfd.readouterr().err
    assert export.current() is None


def test_flush_shares_one_deadline_between_streams(fresh_otel, run_env, receiver, monkeypatch):
    state = export.configure({}, run_env, otel_config(receiver), None)
    monkeypatch.setattr(export, "PROCESSOR_TIMEOUT_SECONDS", 1)
    calls = []

    def slow_test_flush(timeout_millis=30000):
        calls.append("test")
        time.sleep(1.5)
        return True

    def child_flush(timeout_millis=30000):
        calls.append(("child", time.monotonic()))
        return True
    monkeypatch.setattr(state.test_processor, "force_flush", slow_test_flush)
    monkeypatch.setattr(state.forwarder, "force_flush", child_flush)
    started = time.monotonic()
    export.force_flush()
    elapsed = time.monotonic() - started
    assert calls[0] == "test"
    assert calls[1][0] == "child"
    assert elapsed < 1.4


def test_shutdown_makes_the_forwarder_inert_first_and_runs_once(fresh_otel, run_env, receiver):
    state = export.configure({}, run_env, otel_config(receiver), None)
    forwarder = state.forwarder
    export.shutdown()
    assert export.current() is None
    assert forwarder._active is False
    export.shutdown()


def _node(worker, config, status=None):
    output = None if status is None else {plugin.WORKER_STATUS_KEY: status}
    return SimpleNamespace(gateway=SimpleNamespace(id=worker), config=config, workeroutput=output)


def test_controller_repeats_each_workers_losses_once(pytestconfig, capfd):
    config = pytestconfig
    lost = {"missing": 3, "reasons": "export-failure: 3, last OTLP failure: ConnectionError"}
    plugin.record_worker_status(_node("gw0", config, lost), None)
    plugin.record_worker_status(_node("gw0", config, lost), "keyboard-interrupt")  # reported again
    plugin.record_worker_status(_node("gw1", config, {"missing": 0, "reasons": ""}), None)
    crashed = _node("gw2", config)
    del crashed.workeroutput
    plugin.record_worker_status(crashed, "crashed")
    plugin.report_worker_losses(config)
    plugin.report_worker_losses(config)
    err = capfd.readouterr().err
    assert err.count("TEST RESULTS MISSING") == 1
    assert "OpenTelemetry on xdist worker gw0 did not upload 3 test.execution span(s) " \
        "(export-failure: 3, last OTLP failure: ConnectionError)" in err
