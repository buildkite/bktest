import json
import os
import pytest
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
    
from pathlib import Path

def test_only_outer_suite_uploads_not_its_failing_fixtures(tmp_path: Path) -> None:
    """Report the outer suite without uploading its intentional fixture failures."""
    uploads: list[dict] = []

    class UploadHandler(BaseHTTPRequestHandler):
        """Record uploads sent to a loopback endpoint."""

        def do_POST(self) -> None:
            """Accept a test-results upload."""
            uploads.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(202)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, format: str, *args: object) -> None:
            """Suppress HTTP request logging."""

    test_file = Path(__file__).parent / "test_integration_import_error.py"
    name = "test_import_error_captured_in_json_report"
    json_output = tmp_path / "outer-results.json"
    with HTTPServer(("127.0.0.1", 0), UploadHandler) as server:
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            env = {key: value for key, value in os.environ.items()
                   if not key.startswith(("BUILDKITE_ANALYTICS_", "BUILDKITE_TESTS_", "OTEL_"))}
            env.update({
                "BUILDKITE_ANALYTICS_TOKEN": "fake-ci-token",
                "BUILDKITE_ANALYTICS_API_URL": f"http://127.0.0.1:{server.server_port}/v1",
                "BUILDKITE_BUILD_ID": "fixture-isolation-build",
            })
            result = subprocess.run(
                [sys.executable, "-m", "pytest", str(test_file), "-k", name,
                 f"--json={json_output}"],
                env=env, capture_output=True, text=True, check=False, timeout=60,
            )
        finally:
            server.shutdown()
            thread.join()

    assert result.returncode == 0, result.stdout + result.stderr
    results = json.loads(json_output.read_text())
    assert [(test["name"], test["result"]) for test in results] == [(name, "passed")]
    assert len(uploads) == 1, uploads
    assert [(test["name"], test["result"]) for test in uploads[0]["data"]] == [(name, "passed")]
    assert uploads[0]["run_env"]["key"] == "fixture-isolation-build"


def test_add_tag_to_execution_data(tmp_path, fake_env):
    """Verify that tags added via the execution_tag marker are correctly captured in the test data."""
    test_file = Path(__file__).parent / "data" / "test_sample_execution_tag.py"
    json_output_file = tmp_path / "test_results.json"
    
    # Run pytest with our plugin on the test file
    cmd = [
        sys.executable, "-m", "pytest", 
        str(test_file),
        f"--json={json_output_file}",
    ]
    
    # Run pytest in a subprocess
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        print(result.stdout)
        print(result.stderr)
        pytest.fail("pytest run failed, see output above")

    assert json_output_file.exists(), "JSON output file was not created"

    with open(json_output_file, 'r') as f:
        test_results = json.load(f)
    
    tests_by_name = {test["name"]: test for test in test_results}
    
    multi_tag_test = tests_by_name["test_with_multiple_tags"]
    assert "tags" in multi_tag_test
    assert multi_tag_test["tags"] == {
        "language.version": "3.12",
        "team": "backend"
    }
    
    single_tag_test = tests_by_name["test_with_single_tag"] 
    assert "tags" in single_tag_test
    assert single_tag_test["tags"] == {"team": "frontend"}
    
    no_tag_test = tests_by_name["test_without_tags"]
    assert "tags" not in no_tag_test or no_tag_test.get("tags") == {}

def test_location_uses_one_based_line_numbers(tmp_path: Path) -> None:
    """The JSON ``location`` line matches what an editor shows for the test (pytest's is 0-based)."""
    (tmp_path / "test_lines.py").write_text(
        "import pytest\n"
        "\n"
        "def test_on_line_three():\n"
        "    pass\n"
        "\n"
        "class TestGroup:\n"
        "    def test_on_line_seven(self):\n"
        "        pass\n"
        "\n"
        "@pytest.mark.skip\n"
        "def test_decorated_on_line_ten():\n"
        "    pass\n"
    )
    json_output_file = tmp_path / "results.json"
    cmd = [
        sys.executable, "-m", "pytest", "-p", "no:cacheprovider", "test_lines.py",
        f"--json={json_output_file}",
    ]
    result = subprocess.run(cmd, cwd=str(tmp_path), capture_output=True, text=True, check=False)
    assert result.returncode == 0, result.stdout + result.stderr

    locations = {test["name"]: test["location"] for test in json.loads(json_output_file.read_text())}
    assert locations == {
        "test_on_line_three": "test_lines.py:3",
        "test_on_line_seven": "test_lines.py:7",
        "test_decorated_on_line_ten": "test_lines.py:10",
    }


def test_results_follow_pytests_final_verdict_including_teardown_and_reruns(tmp_path: Path) -> None:
    """A failing fixture finalizer fails the test, and ``--reruns`` records one execution per attempt."""
    pytest.importorskip("pytest_rerunfailures")
    test_file = Path(__file__).parent / "data" / "test_sample_settled_results.py"
    json_output_file = tmp_path / "results.json"
    env = {**os.environ, "FLAKY_MARKER": str(tmp_path / "flaky-marker")}
    cmd = [
        sys.executable, "-m", "pytest", "-p", "no:cacheprovider", "--reruns", "1",
        str(test_file), f"--json={json_output_file}",
    ]
    result = subprocess.run(cmd, cwd=str(tmp_path), env=env, capture_output=True, text=True, check=False)
    assert result.returncode == 1, result.stdout + result.stderr
    assert "1 error, 2 rerun" in result.stdout, result.stdout

    executions = [(test["name"], test["result"]) for test in json.loads(json_output_file.read_text())]
    assert executions == [
        ("test_passing_body_with_broken_cleanup", "failed"),  # rerun because teardown failed
        ("test_passing_body_with_broken_cleanup", "failed"),
        ("test_flaky", "failed"),
        ("test_flaky", "passed"),
        ("test_plain_pass", "passed"),
        ("test_plain_skip", "skipped"),
    ]
    broken_cleanup = json.loads(json_output_file.read_text())[0]
    assert "cleanup failed after passing body" in broken_cleanup["failure_reason"]
    assert "no result set" not in result.stderr


@pytest.mark.parametrize("runner_args", [[], ["--forked"]], ids=["pytest", "forked"])
def test_a_failed_test_keeps_its_reason_when_teardown_also_fails(
    tmp_path: Path, runner_args: list[str]
) -> None:
    """Preserve the body failure and append teardown details in the finalized JSON."""
    if runner_args:
        pytest.importorskip("pytest_forked")
        if not hasattr(os, "fork"):
            pytest.skip("pytest-forked requires os.fork")
    test_file = tmp_path / "test_body_and_teardown.py"
    test_file.write_text(
        "import pytest\n"
        "@pytest.fixture\n"
        "def broken_teardown():\n"
        "    yield\n"
        "    raise RuntimeError('teardown exploded')\n"
        "def test_pass_and_teardown_error(broken_teardown):\n"
        "    pass\n"
        "def test_fail_and_teardown_error(broken_teardown):\n"
        "    assert 1 == 2\n"
    )
    json_output_file = tmp_path / "results.json"
    cmd = [
        sys.executable, "-m", "pytest", *runner_args, str(test_file),
        f"--json={json_output_file}",
    ]
    result = subprocess.run(cmd, cwd=str(tmp_path), capture_output=True, text=True, check=False)
    assert result.returncode == 1, result.stdout + result.stderr

    passed_body, failed_body = json.loads(json_output_file.read_text())
    assert passed_body["result"] == "failed"
    assert "RuntimeError: teardown exploded" in "\n".join(passed_body["failure_expanded"][0]["expanded"])
    assert failed_body["result"] == "failed"
    # pytest-forked serializes longrepr to text, whose first line becomes the reason.
    if not runner_args:
        assert failed_body["failure_reason"] == "assert 1 == 2"
    assert failed_body["failure_reason"] != passed_body["failure_reason"]
    body, teardown = failed_body["failure_expanded"]
    assert "assert 1 == 2" in "\n".join(body["expanded"])
    assert "RuntimeError: teardown exploded" in "\n".join(teardown["expanded"])
    assert "no result set" not in result.stdout + result.stderr


class TestTagFiltering:
    import subprocess
    import sys

    def test_filter_by_single_tag(self,tmp_path, fake_env):
        test_file = Path(__file__).parent / "data" / "test_sample_execution_tag_filter.py"
        
        cmd = [
            sys.executable, "-m", "pytest", "--co", "-q", "--tag-filters", "color:red",
            str(test_file),
        ]
        
        # Run pytest in a subprocess
        result = subprocess.run(cmd, cwd=str(tmp_path), capture_output=True, text=True)

        assert "2/5 tests collected" in result.stdout, "collect count mismatch"

        # Parse the output to find collected tests
        lines = result.stdout.strip().splitlines()
        collected_tests = []
        for line in lines:
            if "::" in line:
                collected_tests.append(line)

        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_apple" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_strawberry" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_orange" not in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_banana" not in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_grape" not in collected_tests

    def test_wrong_filter_format(self,tmp_path, fake_env):
        test_file = Path(__file__).parent / "data" / "test_sample_execution_tag_filter.py"
        
        cmd = [
            sys.executable, "-m", "pytest", "--co", "-q", "--tag-filters", "foobar",
            str(test_file),
        ]
        
        # Run pytest in a subprocess
        result = subprocess.run(cmd, cwd=str(tmp_path), capture_output=True, text=True)

        assert "no tests collected" in result.stdout

    def test_no_filter(self,tmp_path, fake_env):
        test_file = Path(__file__).parent / "data" / "test_sample_execution_tag_filter.py"
        
        cmd = [
            sys.executable, "-m", "pytest", "--co", "-q",
            str(test_file),
        ]
        
        # Run pytest in a subprocess
        result = subprocess.run(cmd, cwd=str(tmp_path), capture_output=True, text=True)

        # Parse the output to find collected tests
        lines = result.stdout.strip().splitlines()
        collected_tests = []
        for line in lines:
            if "::" in line:
                collected_tests.append(line)

        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_apple" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_strawberry" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_orange" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_banana" in collected_tests
        assert "tests/buildkite_test_collector/data/test_sample_execution_tag_filter.py::test_grape" in collected_tests
