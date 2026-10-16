"""Persistent pytest runner for `bktec pool exec`.

The runner starts pytest once, then pulls batches from bktec over the runner
protocol and runs each batch in the same pytest session. Each test file is
collected the first time a batch needs it, and session-scoped fixtures are set
up once per job. bktec owns work selection,
retries, muting and the job outcome.
"""

import http.client
import json
import os
import platform
import signal
import socket
import sys
import time
from dataclasses import replace
from importlib.metadata import version
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import quote
from uuid import uuid4

import pytest

# Load the pytest plugin before the collector modules, as pytest does, because
# they import each other.
from .pytest_plugin.logger import logger
from .collector.api import API
from .collector.payload import TestData

SOCKET_ENV = "BUILDKITE_TEST_ENGINE_RUNNER_SOCKET"
REPORT_FORMAT = "pytest-json"
DONE_REASONS = ("plan_completed", "pool_consumed", "pool_errored", "terminating", "error")

# Options that select, filter or stop tests, which bktec controls instead.
INCOMPATIBLE_OPTIONS = {
    "file_or_dir": "file or test arguments",
    "keyword": "-k",
    "markexpr": "-m",
    "deselect": "--deselect",
    "maxfail": "-x/--maxfail",
    "lf": "--lf",
    "stepwise": "--sw",
    "collectonly": "--collect-only",
    "jsonpath": "--json",
    "tag_filters": "--tag-filters",
    "numprocesses": "-n (pytest-xdist)",
    "reruns": "--reruns (pytest-rerunfailures)",
}

JsonDict = Dict[str, Any]
Selector = Tuple[Path, List[str]]


class RunnerError(Exception):
    """A runner protocol or execution error that stops the runner"""


class _Stop(Exception):
    """Raised by the signal handler to interrupt a socket request or wait"""


class _UnixHTTPConnection(http.client.HTTPConnection):  # pylint: disable=too-few-public-methods
    """An HTTP connection over a Unix domain socket"""

    def __init__(self, path: str, timeout: float):
        super().__init__("localhost", timeout=timeout)
        self.socket_path = path

    def connect(self) -> None:
        """Connect to the Unix domain socket"""
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)  # pylint: disable=no-member
        sock.settimeout(self.timeout)
        sock.connect(self.socket_path)
        self.sock = sock


class Client:  # pylint: disable=too-few-public-methods
    """A bktec runner protocol client"""

    def __init__(self, path: str):
        self.path = path
        self.session_id: Optional[str] = None
        self.read_timeout = 35.0
        self.interrupted = False

    def request(self, method: str, path: str, payload: JsonDict, expected: int = 200) -> JsonDict:
        """Send a request, replaying the same body after transport errors"""
        # Serialize once: after a lost response the host accepts only an exact replay.
        body = json.dumps(payload).encode("utf-8")
        headers = {"Content-Type": "application/json", "Connection": "close"}
        # The initial handshake has no session; bktec assigns it in the response.
        if self.session_id:
            headers["X-Bktec-Session"] = self.session_id

        attempts = 0
        while True:
            attempts += 1
            connection = _UnixHTTPConnection(self.path, self.read_timeout)
            try:
                connection.request(method, f"/v1{path}", body=body, headers=headers)
                response = connection.getresponse()
                status, data = response.status, response.read()
            except (OSError, http.client.HTTPException):
                if self.interrupted or attempts >= 3:
                    raise
                continue
            finally:
                connection.close()

            if status != expected:
                raise RunnerError(f"{method} {path}: HTTP {status}")
            value = json.loads(data)
            if not isinstance(value, dict):
                raise RunnerError(f"{method} {path}: expected a JSON object")
            return value


def _nonempty_string(value: Any, name: str) -> str:
    if not isinstance(value, str) or not value:
        raise RunnerError(f"invalid {name}")
    return value


def _selector_value(test: Any) -> str:
    """Map a bktec test case to a pytest selector"""
    if not isinstance(test, dict):
        raise RunnerError("batch test must be an object")
    test_format = test.get("format")
    if test_format == "selector":
        value = test.get("value")
    elif test_format == "file":
        value = test.get("path")
    elif test_format == "example":
        value = test.get("identifier") or test.get("path")
    else:
        raise RunnerError("unsupported selector format")
    value = _nonempty_string(value, "test selector")
    if value.startswith("-"):
        raise RunnerError("test selector cannot be an option")
    return value


def _parse_selector(value: str) -> Selector:
    """Split a pytest selector into an absolute path and node names"""
    path, *names = value.split("::")
    return Path(os.path.abspath(path)), names


def _matches(selector: Selector, path: Path, names: List[str]) -> bool:
    """Match a test like pytest matches its arguments"""
    selector_path, selector_names = selector
    if not selector_names:
        return path == selector_path or selector_path in path.parents
    if path != selector_path or len(names) < len(selector_names):
        return False
    *scope, name = selector_names
    candidate = names[len(scope)]
    # A function name selects all of its parametrized tests.
    return names[:len(scope)] == scope and name in (candidate, candidate.split("[", 1)[0])


class PytestRunner:
    """pytest plugin that runs batches from bktec in one pytest session"""

    # pylint: disable=too-many-instance-attributes

    def __init__(self) -> None:
        self.client: Optional[Client] = None
        self.done = False
        self.stopping: Optional[str] = None
        self.waiting = False
        self.api = API(os.environ)
        self.collection_errors: List[TestData] = []
        self.tests: List[Tuple[pytest.Item, Path, List[str]]] = []
        self.collected: List[Path] = []

    @pytest.hookimpl(tryfirst=True)
    def pytest_configure(self, config: pytest.Config) -> None:
        """pytest_configure hook callback to reject incompatible options before collection"""
        if not os.environ.get(SOCKET_ENV):
            raise pytest.UsageError(
                f"{SOCKET_ENV} is required; run buildkite-pytest with bktec pool exec")

        invalid = [flag for dest, flag in INCOMPATIBLE_OPTIONS.items()
                   if getattr(config.option, dest, None)]
        if invalid:
            raise pytest.UsageError(
                "incompatible options for the persistent runner, as bktec supplies "
                f"and retries tests: {', '.join(invalid)}")

    @pytest.hookimpl(tryfirst=True)
    def pytest_collection(self, session: pytest.Session) -> bool:  # pylint: disable=unused-argument
        """pytest_collection hook callback to collect each batch's files when it runs instead"""
        return True

    @pytest.hookimpl(tryfirst=True)
    def pytest_runtestloop(self, session: pytest.Session) -> bool:
        """pytest_runtestloop hook callback to run batches from bktec instead of all tests"""
        collector = getattr(session.config, "_buildkite", None)
        if collector is None:
            self._write(session, "[buildkite-pytest] the buildkite-test-collector "
                                 "pytest plugin must be enabled")
            return True

        self.client = Client(os.environ[SOCKET_ENV])
        previous = {sig: signal.signal(sig, self._handle_signal)
                    for sig in (signal.SIGINT, signal.SIGTERM)}
        try:
            if not self.api.token:
                logger.warning("No %s environment variable present", API.ENV_TOKEN)
            self._start_session()
            while not self.done:
                self._pull(session, collector)
        except _Stop:
            pass
        except (RunnerError, OSError, http.client.HTTPException, ValueError) as error:
            if not self.stopping:
                self._write(session, f"[buildkite-pytest] {type(error).__name__}: {error}")
        finally:
            self._finish(session, collector)
            for sig, handler in previous.items():
                signal.signal(sig, handler)
        return True

    def _handle_signal(self, signum: int, _frame: Any) -> None:
        self.stopping = signal.Signals(signum).name
        if self.client:
            self.client.interrupted = True
        # Stop a socket request or wait now; a running test finishes first.
        if self.waiting:
            raise _Stop()

    def _collect(self, session: pytest.Session, collector: Any, selectors: List[Selector]) -> None:
        """Collect the selected files that no earlier batch collected"""
        paths: List[Path] = []
        for path, _ in selectors:
            # Missing files leave their tests unreported, which bktec treats as errored.
            if path.exists() and path not in paths and not any(
                    path == done or done in path.parents for done in self.collected):
                paths.append(path)
        if not paths:
            return

        known = {item.nodeid for item, _, _ in self.tests}
        items = session.perform_collect([str(path) for path in paths])
        self.collected.extend(paths)
        self.tests.extend((item, Path(str(item.path)), item.nodeid.split("::")[1:])
                          for item in items if item.nodeid not in known)
        session.testscollected = len(self.tests)
        # Set aside collection errors, which are reported to every batch that selects their file.
        self.collection_errors.extend(collector.payload.data)
        collector.payload = replace(collector.payload, data=())

    def _request(self, method: str, path: str, payload: JsonDict, expected: int = 200) -> JsonDict:
        assert self.client
        self.waiting = True
        try:
            if self.stopping:
                raise _Stop()
            return self.client.request(method, path, payload, expected)
        finally:
            self.waiting = False

    def _start_session(self) -> None:
        response = self._request("POST", "/sessions", {
            "instance_id": str(uuid4()),
            "runner": {
                "name": "buildkite-pytest", "version": version("buildkite-test-collector"),
                "framework": "pytest", "framework_version": pytest.__version__,
                "language": "python", "language_version": platform.python_version(),
                "pid": os.getpid(),
            },
            "capabilities": {"selector_formats": ["selector", "file", "example"]},
        }, expected=201)
        assert self.client
        self.client.session_id = _nonempty_string(response.get("session_id"), "session_id")

        # Allow a little longer than the host's maximum long-poll duration.
        poll = response.get("poll")
        max_wait = poll.get("max_wait_ms") if isinstance(poll, dict) else None
        if type(max_wait) is not int or max_wait < 0:  # pylint: disable=unidiomatic-typecheck
            raise RunnerError("invalid poll max_wait_ms")
        self.client.read_timeout = max_wait / 1000 + 5

    def _pull(self, session: pytest.Session, collector: Any) -> None:
        work = self._request("POST", "/batches", {})
        work_type = work.get("type")
        if work_type == "batch":
            self._run_batch(session, collector, work.get("batch"))
        elif work_type == "wait":
            delay = work.get("retry_after_ms")
            if type(delay) is not int or delay < 0:  # pylint: disable=unidiomatic-typecheck
                raise RunnerError("invalid retry_after_ms")
            self.waiting = True
            try:
                if self.stopping:
                    raise _Stop()
                time.sleep(delay / 1000)
            finally:
                self.waiting = False
        elif work_type == "done":
            if work.get("reason") not in DONE_REASONS:
                raise RunnerError("unknown done reason")
            self.done = True
        else:
            raise RunnerError("unknown work response type")

    def _run_batch(self, session: pytest.Session, collector: Any, batch: Any) -> None:
        if not isinstance(batch, dict):
            raise RunnerError("invalid batch")
        batch_id = _nonempty_string(batch.get("id"), "batch id")
        tests = batch.get("tests")
        if not isinstance(tests, list) or not tests:
            raise RunnerError("batch tests must be a nonempty array")
        values = [_selector_value(test) for test in tests]
        selectors = [_parse_selector(value) for value in values]

        self._write(session, f"[buildkite-pytest] Running batch {batch_id}:")
        for value in values:
            self._write(session, f"[buildkite-pytest]   {value}")

        # Batch IDs are opaque path segments, not URL paths.
        results_path = f"/batches/{quote(batch_id, safe='')}/results"
        try:
            self._collect(session, collector, selectors)
            self._run_tests(session, selectors)
        except _Stop:
            raise
        except Exception as error:  # pylint: disable=broad-except
            if self.stopping:
                raise _Stop() from error
            message = f"pytest execution failed: {error}"
            self._request("POST", results_path, {
                "status": "errored", "report_format": REPORT_FORMAT,
                "error": {"kind": "runner_error", "message": message},
            })
            raise RunnerError(message) from error

        # pytest runs no tests from a file that failed to collect.
        for error in self.collection_errors:
            path = Path(os.path.abspath(error.file_name or ""))
            if any(path == selected or path in selected.parents or selected in path.parents
                   for selected, _ in selectors):
                collector.payload = collector.payload.push_test_data(replace(error, id=uuid4()))

        report = list(collector.payload.as_json()["data"])
        # Upload executions before acknowledging the batch; bktec decides its outcome.
        self._flush(collector)
        self._request("POST", results_path, {"status": "completed", "report_format": REPORT_FORMAT,
                                             "report": report})

    def _run_tests(self, session: pytest.Session, selectors: List[Selector]) -> None:
        items = [item for item, path, names in self.tests
                 if any(_matches(selector, path, names) for selector in selectors)]
        for index, item in enumerate(items):
            # Don't start another test after a signal, such as one received during collection.
            if self.stopping:
                raise _Stop()
            # pytest gives each class test its own instance, and a test can change it,
            # so a retry needs a new one.
            if isinstance(item, pytest.Function) and isinstance(item.parent, pytest.Class):
                item._obj = None  # pylint: disable=protected-access
                item.__dict__.pop("_instance", None)
            # Tear down everything but the session after the batch, so session
            # fixtures stay set up for the next batch.
            nextitem = items[index + 1] if index + 1 < len(items) else session
            item.config.hook.pytest_runtest_protocol(item=item, nextitem=nextitem)
            if session.shouldfail or session.shouldstop:
                raise RunnerError(f"pytest stopped: {session.shouldfail or session.shouldstop}")
        if self.stopping:
            # Partial reports must not acknowledge outstanding work.
            raise _Stop()

    def _flush(self, collector: Any) -> None:
        """Upload and discard the collector's test executions"""
        payload = collector.payload
        collector.payload = replace(payload, data=())
        if payload.data and self.api.token:
            list(self.api.submit(payload))

    def _finish(self, session: pytest.Session, collector: Any) -> None:
        try:
            self._flush(collector)
            # A signal leaves the current batch unresolved rather than accepting partial work.
            if self.stopping and self.client and self.client.session_id:
                self.client.interrupted = False
                self.client.request("DELETE", f"/sessions/{self.client.session_id}",
                                    {"reason": f"signal_{self.stopping[3:].lower()}"})
        except Exception as error:  # pylint: disable=broad-except
            self._write(session, f"[buildkite-pytest] cleanup failed: {error}")
        finally:
            # The runner has uploaded every execution, so the collector has nothing left to send.
            session.config.pluginmanager.unregister(collector)
            del session.config._buildkite  # pylint: disable=protected-access

    @staticmethod
    def _write(session: pytest.Session, message: str) -> None:
        reporter = session.config.pluginmanager.get_plugin("terminalreporter")
        if reporter:
            reporter.write_line(message)
        else:
            print(message, flush=True)


def main(args: Optional[List[str]] = None) -> int:
    """Run pytest as a persistent bktec runner"""
    runner = PytestRunner()
    code = pytest.main(sys.argv[1:] if args is None else args, plugins=[runner])
    if runner.done and code in (pytest.ExitCode.OK, pytest.ExitCode.TESTS_FAILED,
                                pytest.ExitCode.NO_TESTS_COLLECTED):
        # bktec decides the job outcome from the reports.
        return 0
    return int(code) or 1


if __name__ == "__main__":
    sys.exit(main())
