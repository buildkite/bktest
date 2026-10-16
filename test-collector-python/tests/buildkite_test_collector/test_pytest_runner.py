"""Run buildkite-pytest against a fake bktec runner host."""

import json
import os
import signal
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from socketserver import ThreadingMixIn, UnixStreamServer

import pytest

pytestmark = pytest.mark.skipif(sys.platform == "win32", reason="the runner uses a Unix socket")

# Run the module with this interpreter, so it uses the pytest version under test.
RUNNER = [sys.executable, "-m", "buildkite_test_collector.pytest_runner"]

PROJECT = {
    "pytest.ini": "[pytest]\n",
    "conftest.py": """
import os
import pytest

def record(name):
    with open("events.log", "a") as f:
        f.write(f"{name} {os.getpid()}\\n")

@pytest.fixture(scope="session", autouse=True)
def session_fixture():
    record("session_setup")
    yield
    record("session_teardown")
""",
    "test_a.py": """
import os
import pytest
from conftest import record

record("import_a")

@pytest.fixture(scope="module", autouse=True)
def module_fixture():
    record("module_setup")
    yield
    record("module_teardown")

def test_pass():
    pass

def test_flaky():
    # Fails on its first run only, like a test bktec retries.
    first = not os.path.exists("flaky.ran")
    open("flaky.ran", "w").close()
    assert not first

@pytest.mark.parametrize("n", [1, 2])
def test_param(n):
    pass
""",
    "test_b.py": """
def test_b():
    pass
""",
    "test_broken.py": "import does_not_exist\n",
    "test_class.py": """
import os

class TestCounter:
    count = 0

    def test_count(self):
        self.count += 1
        # Fails on its first run only, after changing the instance.
        first = not os.path.exists("class.ran")
        open("class.ran", "w").close()
        assert not first
        assert self.count == 1
""",
    "test_slow.py": """
import time
from conftest import record

record("import_slow")

def test_slow():
    open("slow.started", "w").close()
    time.sleep(1)

def test_after_slow():
    pass
""",
}


class FakeHost(ThreadingMixIn, UnixStreamServer):
    """A scripted bktec runner host that records every request."""

    daemon_threads = True

    def __init__(self, path, pulls, events):
        self.pulls = list(pulls)
        self.events = events
        self.requests = []
        self.drop_first_result = True
        self.hold_pulls = threading.Event()
        super().__init__(str(path), HostHandler)

    def handle_error(self, request, client_address):
        # The runner may hang up on a held long poll when it is signalled.
        pass

    def respond(self, method, path):
        if method == "POST" and path == "/v1/sessions":
            return 201, {"session_id": "s1", "poll": {"max_wait_ms": 1000}}
        if method == "POST" and path == "/v1/batches":
            if not self.pulls:
                # Hold the long poll until the runner is signalled.
                self.hold_pulls.wait(5)
                return 200, {"type": "wait", "retry_after_ms": 0}
            return 200, self.pulls.pop(0)
        if method == "POST" and path.endswith("/results"):
            if self.drop_first_result:
                # Lose the acknowledgement; the runner must replay the same body.
                self.drop_first_result = False
                return None
            return 200, {}
        if method == "DELETE":
            return 200, {}
        return 404, {}


class HostHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        self.handle_request()

    def do_DELETE(self):
        self.handle_request()

    def handle_request(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        self.server.requests.append((self.command, self.path, dict(self.headers), body))
        self.server.events.append(("host", self.command, self.path))
        response = self.server.respond(self.command, self.path)
        if response is None:
            return
        status, value = response
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def address_string(self):
        return "runner"

    def log_message(self, *args):
        pass


class UploadHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.uploads.extend(body["data"])
        self.server.events.append(("upload", [test["name"] for test in body["data"]]))
        self.send_response(202)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


@pytest.fixture
def harness(tmp_path):
    project = tmp_path / "project"
    project.mkdir()
    for name, source in PROJECT.items():
        (project / name).write_text(source)

    events = []
    uploads = HTTPServer(("127.0.0.1", 0), UploadHandler)
    uploads.uploads, uploads.events = [], events
    threading.Thread(target=uploads.serve_forever, daemon=True).start()

    servers = []

    def start(pulls, *args):
        host = FakeHost(tmp_path / "runner.sock", pulls, events)
        servers.append(host)
        threading.Thread(target=host.serve_forever, daemon=True).start()
        env = {key: value for key, value in os.environ.items()
               if not key.startswith(("BUILDKITE", "PYTEST"))}
        env.update({
            "BUILDKITE_TEST_ENGINE_RUNNER_SOCKET": str(tmp_path / "runner.sock"),
            "BUILDKITE_ANALYTICS_TOKEN": "fake",
            "BUILDKITE_ANALYTICS_API_URL": f"http://127.0.0.1:{uploads.server_port}/v1",
        })
        process = subprocess.Popen([*RUNNER, *args], cwd=project, env=env, text=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        return host, process

    yield start, project, uploads, events

    for server in [uploads, *servers]:
        server.shutdown()
        server.server_close()


def batch(batch_id, *tests):
    return {"type": "batch", "batch": {"id": batch_id, "tests": list(tests), "timeout_ms": 60000}}


def results(host):
    """Return each batch's reported tests as {name: result}, by batch ID."""
    reports = {}
    for method, path, _, body in host.requests:
        if method == "POST" and path.endswith("/results"):
            envelope = json.loads(body)
            assert envelope["status"] == "completed"
            assert envelope["report_format"] == "pytest-json"
            reports[path.split("/")[3]] = {f"{test['scope']}::{test['name']}": test["result"]
                                           for test in envelope["report"]}
    return reports


def test_runs_every_batch_in_one_pytest_session(harness):
    start, project, uploads, events = harness
    host, process = start([
        batch("b/1", {"format": "file", "path": "test_a.py"}),
        {"type": "wait", "retry_after_ms": 10},
        batch("b_2",
              {"format": "example", "identifier": "test_a.py::test_flaky"},
              {"format": "example", "path": "test_a.py::test_param"},
              {"format": "selector", "value": "test_b.py"}),
        batch("b_3", {"format": "file", "path": "test_broken.py"}),
        {"type": "done", "reason": "plan_completed"},
    ])
    output, _ = process.communicate(timeout=60)

    assert process.returncode == 0, output
    assert "[buildkite-pytest] Running batch b_2:" in output
    assert "[buildkite-pytest]   test_a.py::test_flaky" in output

    assert results(host) == {
        "b%2F1": {
            "test_a.py::test_pass": "passed",
            "test_a.py::test_flaky": "failed",
            "test_a.py::test_param[1]": "passed",
            "test_a.py::test_param[2]": "passed",
        },
        "b_2": {
            "test_a.py::test_flaky": "passed",
            "test_a.py::test_param[1]": "passed",
            "test_a.py::test_param[2]": "passed",
            "test_b.py::test_b": "passed",
        },
        "b_3": {"::test_broken.py": "failed"},
    }
    broken = [test for test in uploads.uploads if test["name"] == "test_broken.py"]
    assert broken[0]["tags"] == {"test.pytest_collection_error": "true"}

    # Test files are imported when a batch first needs them, and once. There is
    # one session for the job; module fixtures are torn down after every batch.
    log = (project / "events.log").read_text().split()
    assert log[0::2] == ["import_a", "session_setup",
                         "module_setup", "module_teardown",
                         "module_setup", "module_teardown",
                         "session_teardown"]
    assert len(set(log[1::2])) == 1

    # Every request after the handshake uses the session, and the lost
    # acknowledgement was replayed byte for byte without rerunning tests.
    assert all(headers.get("X-Bktec-Session") == "s1" for _, _, headers, _ in host.requests[1:])
    first_results = [body for _, path, _, body in host.requests if path == "/v1/batches/b%2F1/results"]
    assert len(first_results) == 2 and first_results[0] == first_results[1]

    # Each execution is uploaded once, before its batch is acknowledged.
    assert len({test["id"] for test in uploads.uploads}) == len(uploads.uploads) == 9
    assert [event for event in events if event[0] == "upload" or "results" in event[2]] == [
        ("upload", ["test_pass", "test_flaky", "test_param[1]", "test_param[2]"]),
        ("host", "POST", "/v1/batches/b%2F1/results"),
        ("host", "POST", "/v1/batches/b%2F1/results"),
        ("upload", ["test_flaky", "test_param[1]", "test_param[2]", "test_b"]),
        ("host", "POST", "/v1/batches/b_2/results"),
        ("upload", ["test_broken.py"]),
        ("host", "POST", "/v1/batches/b_3/results"),
    ]


def test_retries_a_class_test_with_a_new_instance(harness):
    start, _, _, _ = harness
    host, process = start([
        batch("b_1", {"format": "file", "path": "test_class.py"}),
        batch("b_2", {"format": "example", "identifier": "test_class.py::TestCounter::test_count"}),
        {"type": "done", "reason": "plan_completed"},
    ])
    output, _ = process.communicate(timeout=60)

    assert process.returncode == 0, output
    assert results(host) == {"b_1": {"test_class.py::TestCounter::test_count": "failed"},
                             "b_2": {"test_class.py::TestCounter::test_count": "passed"}}


def wait_for(condition):
    deadline = time.monotonic() + 30
    while not condition():
        assert time.monotonic() < deadline
        time.sleep(0.05)


def test_signal_during_a_test_leaves_the_batch_unresolved(harness):
    start, project, uploads, _ = harness
    host, process = start([batch("b_1", {"format": "file", "path": "test_slow.py"})])
    wait_for((project / "slow.started").exists)
    process.send_signal(signal.SIGTERM)
    output, _ = process.communicate(timeout=60)

    assert process.returncode != 0, output
    # The current test finishes and is uploaded, but the batch is not reported.
    assert [test["name"] for test in uploads.uploads] == ["test_slow"]
    assert [(method, path) for method, path, _, _ in host.requests][-2:] == [
        ("POST", "/v1/batches"), ("DELETE", "/v1/sessions/s1")]
    assert json.loads(host.requests[-1][3]) == {"reason": "signal_term"}


def test_signal_during_a_long_poll_stops_promptly(harness):
    start, _, _, _ = harness
    host, process = start([])
    wait_for(lambda: len(host.requests) == 2)
    process.send_signal(signal.SIGINT)
    output, _ = process.communicate(timeout=3)
    host.hold_pulls.set()

    assert process.returncode != 0, output
    assert [(method, path) for method, path, _, _ in host.requests] == [
        ("POST", "/v1/sessions"), ("POST", "/v1/batches"), ("DELETE", "/v1/sessions/s1")]
    assert json.loads(host.requests[-1][3]) == {"reason": "signal_int"}


@pytest.mark.parametrize("args", [["test_a.py"], ["-k", "flaky"], ["-x"], ["--json=out.json"]])
def test_rejects_options_that_select_or_stop_tests(harness, args):
    start, _, _, _ = harness
    host, process = start([], *args)
    output, _ = process.communicate(timeout=60)

    assert process.returncode == pytest.ExitCode.USAGE_ERROR, output
    assert "incompatible options for the persistent runner" in output
    assert host.requests == []
