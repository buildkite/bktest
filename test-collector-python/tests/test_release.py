"""Check release tag validation and publishing gates without external commands."""

from pathlib import Path
import re
import shutil
import subprocess
import sys

import pytest


PROJECT = Path(__file__).resolve().parents[1]
pytestmark = pytest.mark.skipif(
    sys.version_info < (3, 11), reason="The release container uses Python 3.13 with tomllib"
)


@pytest.fixture
def release_project(tmp_path: Path) -> Path:
    """Copy the release script into an isolated project with offline tools."""
    steps = tmp_path / ".buildkite" / "steps"
    steps.mkdir(parents=True)
    shutil.copy(PROJECT / ".buildkite/steps/release", steps / "release")
    tools = tmp_path / "bin"
    tools.mkdir()
    command = tools / "command"
    command.write_text('#!/bin/bash\nprintf "%s %s\\n" "${0##*/}" "$*" >> "$HOME/commands"\n')
    command.chmod(0o755)
    for name in ("apt-get", "curl", "uv", "uvx"):
        (tools / name).symlink_to(command)
    (tools / "python3").symlink_to(sys.executable)
    (tmp_path / ".local/bin").mkdir(parents=True)
    (tmp_path / ".local/bin/env").touch()
    return tmp_path


@pytest.mark.parametrize(
    "version,tag,publish,token,expected",
    [
        ("1.10.0", "test-collector-python/v1.10.0", False, True, "rehearsal"),
        ("1.10.0a1", "test-collector-python/v1.10.0a1", False, True, "rehearsal"),
        ("1.10.0b2", "test-collector-python/v1.10.0b2", False, True, "rehearsal"),
        ("1.10.0rc3", "test-collector-python/v1.10.0rc3", False, True, "rehearsal"),
        ("1.10.0b2", "", False, True, "rehearsal"),
        ("1.10.0b2", "test-collector-python/v1.10.0b1", False, False, "mismatch"),
        ("1.10.0b2", "test-collector-python/v1.10.0-beta.2", True, True, "invalid"),
        ("1.10.0b2", "test-collector-python/v1.10.0b", True, True, "invalid"),
        ("1.10.0b2", "test-collector-python/v1.10.0b2junk", True, True, "invalid"),
        ("1.10.0b2", "test-collector-ruby/v1.10.0b2", True, True, "invalid"),
        ("1.10.0b2", "v1.10.0b2", True, True, "invalid"),
        ("1.10.0b2", "", True, True, "missing tag"),
        ("1.10.0b2", "test-collector-python/v1.10.0b2", True, False, "missing token"),
        ("1.10.0b2", "test-collector-python/v1.10.0b2", True, True, "upload"),
        ("1.10.0", "test-collector-python/v1.10.0", True, True, "upload"),
    ],
)
def test_release_gates(
    release_project: Path, version: str, tag: str, publish: bool, token: bool, expected: str
) -> None:
    """Require matching tags and explicit approval before invoking an upload."""
    (release_project / "pyproject.toml").write_text(f'[project]\nversion = "{version}"\n')
    result = subprocess.run(
        ["/bin/bash", str(release_project / ".buildkite/steps/release")],
        env={
            "PATH": f"{release_project / 'bin'}:/usr/bin:/bin",
            "HOME": str(release_project),
            "BUILDKITE_TAG": tag,
            "RELEASE_PUBLISH": "true" if publish else "false",
            "TWINE_PASSWORD": "fake-offline-token" if token else "",
        },
        capture_output=True,
        text=True,
        check=False,
    )
    assert (result.returncode == 0) == (expected in ("rehearsal", "upload")), result.stderr
    commands = release_project / "commands"
    calls = commands.read_text() if commands.exists() else ""
    assert ("twine upload" in calls) == (expected == "upload")
    if expected == "rehearsal":
        assert "uv build" in calls and "twine check" in calls
        assert "Rehearsal only" in result.stdout
    elif expected == "mismatch":
        assert "tag says 1.10.0b1 but pyproject.toml says 1.10.0b2" in result.stderr
    elif expected == "invalid":
        assert "does not match" in result.stderr
    elif expected == "missing tag":
        assert "without a" in result.stderr
    elif expected == "missing token":
        assert "TWINE_PASSWORD not set" in result.stderr

    if tag:
        pipeline = (PROJECT / ".buildkite/pipeline.release.yml").read_text()
        conditions = re.findall(r"if: build.tag =~ /(.*)/", pipeline)
        assert len(conditions) == 2
        for condition in conditions:
            assert bool(re.fullmatch(condition.replace(r"\/", "/"), tag)) == (
                expected != "invalid"
            )
