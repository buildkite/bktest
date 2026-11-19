#!/usr/bin/env python3
"""Exercise release guards and resume against a local directory, never a service."""

import argparse
import json
from pathlib import Path
import re
import shutil

from stage import VERSION, digest


def rehearse(stage, destination, tag, source, approve, advance_stable):
    manifest_path = stage / "manifest.json"
    manifest = json.loads(manifest_path.read_text())
    version = manifest["version"]
    if not approve or not re.fullmatch(VERSION, version) or tag != f"v{version}":
        raise ValueError("approval and an exact CLI release tag are required")
    if not re.fullmatch(r"[a-f0-9]{40}", source) or source != manifest["source"]:
        raise ValueError("tag source does not match the staging source")
    prerelease = "-" in version
    if advance_stable and prerelease:
        raise ValueError("prereleases cannot advance stable channels")

    # Preflight both the staging inventory and the destination before any write.
    # The version marker reserves a version and makes retries use the same inputs.
    root = destination / tag
    files = {"manifest.json": digest(manifest_path)}
    for name, sha in manifest["sha256"].items():
        path = Path(name)
        if path.is_absolute() or ".." in path.parts or (stage / path).is_symlink():
            raise ValueError(f"not a staged artifact: {name}")
        if digest(stage / path) != sha:
            raise ValueError(f"staged hash differs: {name}")
        # Limit prereleases to the two GitHub releases until each package
        # destination's prerelease visibility and channel behavior is verified.
        if path.parts[0] == "github" or (not prerelease and path.parts[0] != "tap"):
            files[name] = sha

    if root.exists() and not (root / "manifest.json").is_file():
        raise ValueError("version already exists without a reservation manifest")
    for name, sha in files.items():
        existing = root / name
        if existing.exists() and digest(existing) != sha:
            raise ValueError(f"version already exists with a different hash: {name}")

    for name in files:
        target = root / name
        if not target.exists():
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as output, (stage / name).open("rb") as data:
                shutil.copyfileobj(data, output)

    if advance_stable:
        # A single local pointer models the grouped mutable moves after the
        # versioned inventory is complete; it is NOT a remote channel adapter.
        (destination / "stable.txt").write_text(f"{tag}\n")
    return len(files)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", type=Path)
    parser.add_argument("destination", type=Path)
    parser.add_argument("--tag", required=True, help="simulated Buildkite build.tag, not build.branch")
    parser.add_argument("--source", required=True, help="simulated peeled Git tag SHA")
    parser.add_argument("--approve", action="store_true", help="simulate publish approval")
    parser.add_argument("--advance-stable", action="store_true", help="separately approve a fake stable move")
    args = parser.parse_args()
    try:
        count = rehearse(args.stage, args.destination, args.tag, args.source, args.approve, args.advance_stable)
    except (OSError, ValueError) as error:
        parser.exit(1, f"{error}\n")
    print(f"Verified {count} files in the local destination")
