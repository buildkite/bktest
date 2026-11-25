#!/usr/bin/env python3
"""Stage both CLI distributions from six already-built payloads. No publishing."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

NUMBER = r"(?:0|[1-9][0-9]*)"
IDENTIFIER = rf"(?:{NUMBER}|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)"
VERSION = rf"3\.{NUMBER}\.{NUMBER}(?:-{IDENTIFIER}(?:\.{IDENTIFIER})*)?"
TARGETS = [(os, arch) for os in ("darwin", "linux", "windows") for arch in ("amd64", "arm64")]


def digest(path):
    with path.open("rb") as data:
        return hashlib.file_digest(data, "sha256").hexdigest()


def native_path(package, version, arch, fmt):
    return f"packages/{fmt}/{package}_{version}_linux_{arch}.{fmt}"


def stage(version, source, payloads, out):
    if not re.fullmatch(VERSION, version) or not re.fullmatch(r"[a-f0-9]{40}", source):
        raise ValueError("expected a CLI v3 version (without v) and full source SHA")
    targets = {}
    for os, arch in TARGETS:
        ext = ".exe" if os == "windows" else ""
        path = payloads / f"bktest_{version}_{os}_{arch}{ext}"
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError(f"missing or empty payload: {path}")
        targets[os, arch] = path, ext

    out.mkdir(parents=True)
    assets = {}

    def write(relative, *, text=None, copy=None):
        path = out / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        if copy is not None:
            shutil.copyfile(copy, path)
            if path.suffix not in (".deb", ".rpm"):
                path.chmod(0o755)
        else:
            path.write_text(text, encoding="utf-8")
        assets[relative] = digest(path)
        return path

    formula = [
        '# frozen_string_literal: true', '', 'class Bktec < Formula',
        '  desc "Buildkite Test Engine Client"',
        '  homepage "https://github.com/buildkite/bktest"', f'  version "{version}"',
    ]
    for os, brew_os in (("darwin", "macos"), ("linux", "linux")):
        formula.append(f"\n  on_{brew_os} do")
        for arch, cpu in (("amd64", "intel"), ("arm64", "arm")):
            name = f"bktec_{version}_{os}_{arch}"
            url = f"https://github.com/buildkite/test-engine-client/releases/download/v{version}/{name}"
            formula.extend([
                f"    on_{cpu} do", f'      url "{url}"',
                f'      sha256 "{digest(targets[os, arch][0])}"',
                "      def install", f'        bin.install "{name}" => "bktec"',
                '        bin.install_symlink bin/"bktec" => "bktest"', "      end", "    end",
            ])
        formula.append("  end")
    formula.extend([
        "", "  test do", f'    assert_match "v{version}", shell_output("#{{bin}}/bktec --version")',
        f'    assert_match "v{version}", shell_output("#{{bin}}/bktest --version")', "  end", "end", "",
    ])

    for (os, arch), (payload, ext) in targets.items():
        write(f"github/bktest/{payload.name}", copy=payload)
        write(f"github/test-engine-client/bktec_{version}_{os}_{arch}{ext}", copy=payload)
        write(f"files/bktec-{os}-{arch}-{version}{ext or '.bin'}", copy=payload)
        if os == "linux":
            context = f"image/linux_{arch}"
            dockerfile = Path(__file__).resolve().parents[1] / "Dockerfile"
            write(f"{context}/Dockerfile", text=dockerfile.read_text())
            for name in ("bktest", "bktec"):
                write(f"{context}/{name}", copy=payload)
            package = dict(
                name="bktec", arch=arch, version=version, vendor="Buildkite",
                homepage="https://github.com/buildkite/bktest", maintainer="Buildkite <support@buildkite.com>",
                description="Buildkite Test Engine Client", license="MIT", provides=["bktec"],
                contents=[dict(src=str(payload.resolve()), dst=f"/usr/bin/{name}", file_info={"mode": 493})
                          for name in ("bktest", "bktec")],
            )
            metapackage = dict(package, name="bktest", provides=[], contents=[], overrides={
                "deb": {"depends": ["bktec (>= 3.6.0)"]},
                "rpm": {"depends": ["bktec >= 3.6.0"]},
            })
            with tempfile.TemporaryDirectory(dir=out) as temp:
                config = Path(temp) / "nfpm.yaml"
                for metadata in (package, metapackage):
                    config.write_text(json.dumps(metadata))  # JSON is valid YAML.
                    for fmt in ("deb", "rpm"):
                        relative = native_path(metadata["name"], version, arch, fmt)
                        path = out / relative
                        path.parent.mkdir(parents=True, exist_ok=True)
                        subprocess.run(["nfpm", "package", "--config", config, "--packager", fmt,
                                        "--target", path], check=True)
                        assets[relative] = digest(path)
                        if metadata is package:
                            write(f"github/test-engine-client/{path.name}", copy=path)

    for directory, name in (("github/bktest", f"bktest_{version}_checksums.txt"),
                            ("github/test-engine-client", f"bktec_{version}_checksums.txt"),
                            ("files", f"bktec-checksums-{version}.txt")):
        write(f"{directory}/{name}", text="".join(
            f"{sha}  {Path(path).name}\n" for path, sha in sorted(assets.items())
            if path.startswith(directory + "/")
        ))
    write("tap/Formula/bktec.rb", text="\n".join(formula))
    alias = out / "tap/Aliases/bktest"
    alias.parent.mkdir()
    alias.symlink_to("../Formula/bktec.rb")
    manifest = dict(version=version, source=source, sha256=assets,
                    symlinks={"tap/Aliases/bktest": "../Formula/bktec.rb"})
    (out / "manifest.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("payloads", type=Path)
    parser.add_argument("out", type=Path)
    args = parser.parse_args()
    stage(args.version, args.source, args.payloads, args.out)
