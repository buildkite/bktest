"""End-to-end OTel mode: run the sample suite in a subprocess against a loopback receiver."""

import json
import re
from pathlib import Path

import pytest

from otel_harness import DATA_DIR, otel_env, run_pytest

EXPECTED_RESULTS = {
    "test_passing": "pass",
    "test_failing": "fail",
    "test_skipped": "skipped",
    "test_expected_failure": "skipped",
    "test_setup_error": "fail",
    "test_annotated": "pass",
    "test_with_child_span": "pass",
}
SAMPLE_EXIT_CODE = 1  # the sample has failing tests


def _by_name(spans):
    return {span.attributes["buildkite.test.name"]: span for span in spans}


def _assert_synthesis_prerequisites(span, run_key):
    assert span.name == "test.execution"
    assert span.parent_span_id == b""
    assert span.attributes["buildkite.execution.via"] == "otlp"
    assert span.attributes["buildkite.test.name"]
    assert span.attributes["buildkite.run_key"] == run_key
    assert span.attributes["test.case.result.status"] in ("pass", "fail", "skipped")
    assert 0 < span.start_time_unix_nano <= span.end_time_unix_nano
    assert isinstance(span.attributes["code.line.number"], int)


def test_sample_suite_arrives_as_test_execution_spans(tmp_path, receiver):
    run = run_pytest(tmp_path, otel_env(receiver))
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    test_requests = receiver.trace_requests("test")
    assert test_requests, run.stderr
    for request in test_requests:
        assert request.header("Authorization") == 'Token token="otlp-token"'
        assert request.header("Buildkite-Tests-Run-Key") == "build-1234"
        assert request.content_type == "application/x-protobuf"
        assert request.encoding == "gzip"

    spans = receiver.spans("test")
    assert len(spans) == len(EXPECTED_RESULTS)
    by_name = _by_name(spans)
    assert {name: span.attributes["test.case.result.status"] for name, span in by_name.items()} \
        == EXPECTED_RESULTS
    for span in spans:
        _assert_synthesis_prerequisites(span, "build-1234")
        assert span.attributes["code.file.path"].endswith("test_sample_otel.py")
        assert span.resource["cicd.pipeline.run.id"] == "build-1234"
        assert span.resource["cicd.pipeline.run.url.full"] == "https://buildkite.com/acme/app/builds/1"
        assert span.resource["cicd.worker.id"] == "agent-7"
        assert "buildkite.run_url" not in span.attributes
        assert span.attributes["buildkite.collector.version"]
    worker_tags = {name: span.attributes["buildkite.tag.ci.worker.id"] for name, span in by_name.items()}
    assert worker_tags.pop("test_annotated") == "per-test"
    assert set(worker_tags.values()) == {"agent-7"}

    failing = by_name["test_failing"]
    assert failing.status_code == 2
    assert "one is not two" in failing.status_message
    exception_events = [event for event in failing.events if event["name"] == "exception"]
    assert exception_events and "one is not two" in exception_events[0]["exception.message"]
    assert "test_sample_otel.py" in exception_events[0]["exception.stacktrace"]

    setup_error = by_name["test_setup_error"]
    assert setup_error.status_code == 2
    assert any("fixture exploded" in event.get("exception.message", "") for event in setup_error.events)

    assert by_name["test_skipped"].status_code == 0
    assert by_name["test_passing"].status_code == 0
    assert by_name["test_passing"].end_time_unix_nano - by_name["test_passing"].start_time_unix_nano \
        >= 10_000_000

    annotated = by_name["test_annotated"]
    assert annotated.attributes["buildkite.tag.team"] == "backend"
    assert [event for event in annotated.events if event["name"] == "test.annotation"] == [
        {"name": "test.annotation", "buildkite.annotation": "hello from the test"}
    ]

    assert receiver.upload_requests() == []
    assert "TEST RESULTS MISSING" not in run.stderr


def test_child_spans_and_phases_use_the_child_stream(tmp_path, receiver):
    run = run_pytest(tmp_path, otel_env(receiver))
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    for request in receiver.trace_requests("child"):
        assert request.header("Authorization") == 'Token token="otlp-token"'
        assert request.header("Buildkite-Tests-Run-Key") == "build-1234"
        assert all(span.name != "test.execution" for span in request.spans)
    for request in receiver.trace_requests("test"):
        assert all(span.name == "test.execution" for span in request.spans)

    test_spans = _by_name(receiver.spans("test"))
    child_spans = receiver.spans("child")
    by_trace = {}
    for span in child_spans:
        by_trace.setdefault(span.trace_id, []).append(span)

    def children_of(test_name):
        return by_trace.get(test_spans[test_name].trace_id, [])

    child_span_test = {span.name: span for span in children_of("test_with_child_span")}
    assert set(child_span_test) == {"db.query", "test.body"}
    assert child_span_test["test.body"].parent_span_id == test_spans["test_with_child_span"].span_id
    assert child_span_test["db.query"].parent_span_id == child_span_test["test.body"].span_id
    assert child_span_test["test.body"].status_code == 0

    failing = {span.name: span for span in children_of("test_failing")}
    assert set(failing) == {"test.body"}
    assert failing["test.body"].status_code == 2
    assert "one is not two" in failing["test.body"].status_message

    setup_error = {span.name: span for span in children_of("test_setup_error")}
    assert set(setup_error) == {"test.setup"}
    assert setup_error["test.setup"].status_code == 2

    assert children_of("test_passing") == []
    assert children_of("test_skipped") == []
    assert {span.name for span in children_of("test_expected_failure")} == {"test.body"}


def test_span_identity_matches_the_json_upload(tmp_path, receiver):
    run = run_pytest(tmp_path, otel_env(receiver))
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    json_tests = json.loads(run.json_path.read_text())
    assert len(json_tests) == len(EXPECTED_RESULTS)
    json_identity = {
        (test["scope"], test["name"], test["file_name"], test["location"], test["result"])
        for test in json_tests
    }
    span_identity = {
        (
            span.attributes["buildkite.test.scope"],
            span.attributes["buildkite.test.name"],
            span.attributes["code.file.path"],
            f'{span.attributes["code.file.path"]}:{span.attributes["code.line.number"]}',
            {"pass": "passed", "fail": "failed", "skipped": "skipped"}[
                span.attributes["test.case.result.status"]
            ],
        )
        for span in receiver.spans("test")
    }
    assert span_identity == json_identity
    for span in receiver.spans("test"):
        assert span.attributes["test.case.name"] == \
            f'{span.attributes["buildkite.test.scope"]}::{span.attributes["buildkite.test.name"]}'
        assert span.attributes["test.suite.name"] == span.attributes["buildkite.test.scope"]


def test_retried_requests_keep_their_stream_header(tmp_path, receiver):
    receiver.next_statuses = [503]
    run = run_pytest(tmp_path, otel_env(receiver))
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    test_requests = receiver.trace_requests("test")
    assert [request.status for request in test_requests][:2] == [503, 200]
    assert len(receiver.spans("test")) >= len(EXPECTED_RESULTS)
    for request in receiver.trace_requests():
        streams = {("test" if span.name == "test.execution" else "child") for span in request.spans}
        assert streams == {request.header("Buildkite-Tests-Span-Stream")}
    assert "TEST RESULTS MISSING" not in run.stderr


def test_standard_otel_env_variables_are_ignored(tmp_path, receiver):
    env = otel_env(
        receiver,
        OTEL_EXPORTER_OTLP_HEADERS="authorization=garbage,x-env-only=1",
        OTEL_EXPORTER_OTLP_TRACES_HEADERS="authorization=garbage2,x-env-traces=1",
        OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="http/json",
        OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:1",
        OTEL_EXPORTER_OTLP_COMPRESSION="none",
        OTEL_TRACES_SAMPLER="always_off",
        OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT="2",
        OTEL_BSP_MAX_QUEUE_SIZE="1",
    )
    run = run_pytest(tmp_path, env)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    spans = receiver.spans("test")
    assert len(spans) == len(EXPECTED_RESULTS)
    assert {name: span.attributes["test.case.result.status"] for name, span in _by_name(spans).items()} \
        == EXPECTED_RESULTS
    for span in spans:
        _assert_synthesis_prerequisites(span, "build-1234")
    for request in receiver.trace_requests():
        assert request.content_type == "application/x-protobuf"
        assert request.encoding == "gzip"
        assert request.header("Authorization") == 'Token token="otlp-token"'
        assert not [name for name in request.headers if name.startswith("x-env")]


def test_otel_sdk_disabled_does_not_lose_test_results(tmp_path, receiver):
    run = run_pytest(tmp_path, otel_env(receiver, OTEL_SDK_DISABLED="true"))
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    spans = receiver.spans("test")
    assert {name: span.attributes["test.case.result.status"] for name, span in _by_name(spans).items()} \
        == EXPECTED_RESULTS
    assert "TEST RESULTS MISSING" not in run.stderr
    assert receiver.upload_requests() == []
    # The global provider still honours the switch, so phases and children stay home.
    assert receiver.spans("child") == []


def test_queue_overflow_warns_about_missing_results(tmp_path, receiver):
    receiver.first_request_delay = 2.0
    env = otel_env(
        receiver,
        BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE="2",
        BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE="2",
    )
    run = run_pytest(tmp_path, env)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    received = len(receiver.spans("test"))
    assert received < len(EXPECTED_RESULTS)
    dropped = len(EXPECTED_RESULTS) - received
    assert f"TEST RESULTS MISSING: OpenTelemetry dropped {dropped} test.execution span(s)" in run.stderr
    assert "not-exported" in run.stderr
    assert run.stderr.count("TEST RESULTS MISSING: OpenTelemetry dropped") == 1
    assert receiver.upload_requests() == []


def test_export_failure_names_the_http_status(tmp_path, receiver):
    receiver.next_statuses = [400] * 20
    run = run_pytest(tmp_path, otel_env(receiver), merge_output=True)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stdout

    warning = f"TEST RESULTS MISSING: OpenTelemetry dropped {len(EXPECTED_RESULTS)} test.execution span(s)"
    assert warning in run.stdout
    assert "export-failure: 7, last OTLP failure: 400" in run.stdout
    # The warning must start its own line, not be glued onto pytest's progress line.
    assert "[100%]\n" in run.stdout
    assert "[100%]buildkite-test-collector" not in run.stdout
    assert receiver.upload_requests() == []


def test_xdist_workers_report_missing_results_before_the_controller_stops_them(tmp_path, receiver):
    # Each worker exports for itself. The controller kills a worker 10 seconds
    # after it reports its session finished, so a flush that outlives that
    # (here: every request takes 15 seconds, then fails) must warn first.
    pytest.importorskip("xdist")
    receiver.request_delay = 15.0
    receiver.next_statuses = [400] * 20
    run = run_pytest(tmp_path, otel_env(receiver), "-n", "2")
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    dropped = re.findall(r"TEST RESULTS MISSING: OpenTelemetry dropped (\d+) test\.execution", run.stderr)
    assert sum(int(count) for count in dropped) == len(EXPECTED_RESULTS), run.stderr
    assert receiver.upload_requests() == []


def test_invalid_run_key_falls_back_to_json_before_touching_the_sdk(tmp_path, receiver):
    env = otel_env(receiver, BUILDKITE_BUILD_ID="bad key")
    run = run_pytest(tmp_path, env)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    assert "OpenTelemetry span export disabled: run key 'bad key' is invalid" in run.stderr
    assert "BUILDKITE_ANALYTICS_OTEL_ENABLED is set, but OpenTelemetry could not be configured" \
        in run.stderr
    assert "uploading results as JSON instead" in run.stderr
    assert receiver.trace_requests() == []
    assert len(receiver.upload_requests()) == 1
    assert receiver.upload_requests()[0].header("Authorization") == 'Token token="json-token"'


def test_invalid_endpoint_falls_back_to_json(tmp_path, receiver):
    env = otel_env(receiver, BUILDKITE_ANALYTICS_OTLP_ENDPOINT="")
    run = run_pytest(tmp_path, env)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    assert "OpenTelemetry span export disabled: ValueError: BUILDKITE_ANALYTICS_OTLP_ENDPOINT " \
        "must be an http(s) URL, got ''" in run.stderr
    assert "uploading results as JSON instead" in run.stderr
    assert receiver.trace_requests() == []
    assert len(receiver.upload_requests()) == 1


def test_a_test_whose_span_could_not_start_is_reported_missing_once(tmp_path, receiver):
    (tmp_path / "failing_start.py").write_text(
        "from buildkite_test_collector.pytest_plugin.otel import spans\n"
        "original = spans.start_test_span\n"
        "def start_test_span(state, test):\n"
        "    if test.name == 'test_passing':\n"
        "        spans.record_start_failure(RuntimeError('boom'))\n"
        "        return None\n"
        "    return original(state, test)\n"
        "spans.start_test_span = start_test_span\n"
    )
    env = otel_env(receiver, PYTHONPATH=str(tmp_path))
    run = run_pytest(tmp_path, env, "-p", "failing_start")
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    assert run.stderr.count("TEST RESULTS MISSING") == 1
    assert "dropped 1 test.execution span(s) (could not start span: RuntimeError: boom)" in run.stderr
    by_name = _by_name(receiver.spans("test"))
    assert set(by_name) == set(EXPECTED_RESULTS) - {"test_passing"}


def test_missing_sdk_falls_back_to_json(tmp_path, receiver):
    shim = tmp_path / "shim" / "opentelemetry"
    shim.mkdir(parents=True)
    (shim / "__init__.py").write_text('raise ImportError("no opentelemetry here")\n')
    env = otel_env(receiver, PYTHONPATH=str(tmp_path / "shim"), BUILDKITE_ANALYTICS_TOKEN=None)
    run = run_pytest(tmp_path, env, test_file=DATA_DIR / "test_sample_skip.py")
    assert run.returncode == 0, run.stderr

    assert "OpenTelemetry span export disabled: ImportError: no opentelemetry here" in run.stderr
    assert "results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set" in run.stderr
    assert receiver.trace_requests() == []
    assert receiver.upload_requests() == []
    assert "No BUILDKITE_ANALYTICS_TOKEN environment variable present" in run.stderr


def test_no_credential_uses_the_json_path_silently(tmp_path, receiver):
    env = otel_env(receiver, BUILDKITE_TESTS_OTLP_TOKEN=None, BUILDKITE_ANALYTICS_TOKEN=None)
    run = run_pytest(tmp_path, env, test_file=DATA_DIR / "test_sample_skip.py")
    assert run.returncode == 0, run.stderr

    assert "OpenTelemetry" not in run.stderr
    assert receiver.trace_requests() == []


def test_otel_off_uses_only_the_json_path(tmp_path, receiver):
    env = otel_env(receiver, BUILDKITE_ANALYTICS_OTEL_ENABLED=None)
    run = run_pytest(tmp_path, env, test_file=DATA_DIR / "test_sample_skip.py")
    assert run.returncode == 0, run.stderr

    assert receiver.trace_requests() == []
    assert len(receiver.upload_requests()) == 1
    assert "OpenTelemetry" not in run.stderr


def test_span_filter_hook_drops_child_spans_but_not_phases_or_tests(tmp_path, receiver):
    conftest = tmp_path / "conftest.py"
    conftest.write_text(
        "def pytest_buildkite_otel_span_filter(span):\n"
        "    return span.name != 'db.query'\n"
    )
    sample = tmp_path / "test_sample_otel.py"
    sample.write_text(Path(DATA_DIR / "test_sample_otel.py").read_text())
    run = run_pytest(tmp_path, otel_env(receiver), test_file=sample)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    assert len(receiver.spans("test")) == len(EXPECTED_RESULTS)
    child_names = [span.name for span in receiver.spans("child")]
    assert "db.query" not in child_names
    assert child_names.count("test.body") == 3


def test_span_filter_hook_from_a_conftest_loaded_during_collection(tmp_path, receiver):
    # Only conftests at or above the command-line paths load before the session
    # starts; this one is found while collecting.
    nested = tmp_path / "tests" / "nested"
    nested.mkdir(parents=True)
    (nested / "conftest.py").write_text(
        "def pytest_buildkite_otel_span_filter(span):\n"
        "    return span.name != 'db.query'\n"
    )
    (nested / "test_sample_otel.py").write_text(Path(DATA_DIR / "test_sample_otel.py").read_text())
    run = run_pytest(tmp_path, otel_env(receiver), test_file=tmp_path)
    assert run.returncode == SAMPLE_EXIT_CODE, run.stderr

    assert len(receiver.spans("test")) == len(EXPECTED_RESULTS)
    assert "db.query" not in [span.name for span in receiver.spans("child")]
