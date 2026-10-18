import json
import os
import pytest
import subprocess
import sys
    
from pathlib import Path

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

def test_location_uses_one_based_line_numbers(tmp_path):
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


def test_results_follow_pytests_final_verdict_including_teardown_and_reruns(tmp_path):
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
