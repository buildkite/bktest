"""Publish an approved, retained OCI index to an existing exact-version route.

Requires Skopeo and umoci. An approval must pin both the stage archive SHA-256
and the OCI index digest. Retain the unpacked OCI layout unchanged for retries;
its index.json must select one two-platform OCI index, with no attestation
descriptors interleaved with the platform images. Only stable v<version> tags
are written, not architecture-suffixed tags or mutable channels.
Use an immutable version tag policy or an exclusive writer: OCI Distribution
does not offer a portable conditional create for manifest tags.
"""

from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from tempfile import TemporaryDirectory
from typing import TYPE_CHECKING

from stage import VERSION, digest

if TYPE_CHECKING:
    from publish import Stage


EXISTING_REPOSITORIES = (
    "docker.io/buildkite/test-engine-client",
    "public.ecr.aws/buildkite/test-engine-client",
    "packages.buildkite.com/buildkite/test-engine-client-docker/test-engine-client",
)
INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
SHA256 = r"[0-9a-f]{64}"


def _run(*command: str, loopback: bool = False) -> bytes:
    env = ({name: value for name, value in os.environ.items() if not name.lower().endswith("_proxy")}
           if loopback else None)
    result = subprocess.run(command, capture_output=True, env=env)
    if result.returncode:
        raise OSError(f"{command[0]} {command[1]} failed: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout


def _blob(layout: Path, descriptor: dict) -> Path:
    match = re.fullmatch(f"sha256:({SHA256})", descriptor["digest"])
    if not match:
        raise ValueError("expected a SHA-256 OCI descriptor")
    path = layout / "blobs/sha256" / match[1]
    if path.stat().st_size != descriptor["size"] or digest(path) != match[1]:
        raise ValueError(f"OCI blob differs: {descriptor['digest']}")
    return path


@dataclass(frozen=True)
class OCIDestination:
    name: str  # Fully qualified legacy repository (no tag).
    layout: Path
    index_digest: str  # sha256:<hex> from approval, not discovered from the layout.
    archive_sha256: str  # Bare hexadecimal stage archive hash from the same approval.

    def _loopback(self) -> bool:
        pattern = r"127\.0\.0\.1:[0-9]+/buildkite/(?:test-engine-client-docker/)?test-engine-client"
        return re.fullmatch(pattern, self.name) is not None

    def _inventory(self, stage: "Stage") -> tuple[str, dict]:
        if self.name not in EXISTING_REPOSITORIES and not self._loopback():
            raise ValueError(f"not an existing OCI destination: {self.name}")
        if not re.fullmatch(VERSION, stage.version) or "-" in stage.version:
            raise ValueError("OCI publication requires an exact stable CLI version")
        if not re.fullmatch(SHA256, self.archive_sha256) or self.archive_sha256 != stage.archive_sha256:
            raise ValueError("OCI approval does not match the stage archive")
        try:
            top = self._verify_layout(stage)
        except (KeyError, TypeError, AttributeError) as error:
            raise ValueError("malformed OCI layout or stage inventory") from error
        return f"{self.name}:v{stage.version}", top

    def _verify_layout(self, stage: "Stage") -> dict:
        if ":" in str(self.layout.resolve()):
            raise ValueError("OCI layout path cannot contain a colon")
        if json.loads((self.layout / "oci-layout").read_bytes())["imageLayoutVersion"] != "1.0.0":
            raise ValueError("expected an OCI image layout")
        header = json.loads((self.layout / "index.json").read_bytes())
        top, = header["manifests"]
        if header["schemaVersion"] != 2 or top["mediaType"] != INDEX or top["digest"] != self.index_digest:
            raise ValueError("OCI index differs from the approved index")
        index = json.loads(_blob(self.layout, top).read_bytes())
        if index["schemaVersion"] != 2 or index["mediaType"] != INDEX:
            raise ValueError("expected an OCI image index")
        children = index["manifests"]
        platforms = [(child["platform"]["os"], child["platform"]["architecture"]) for child in children]
        if sorted(platforms) != [("linux", "amd64"), ("linux", "arm64")]:
            raise ValueError("expected exactly linux/amd64 and linux/arm64 in the OCI index")
        with TemporaryDirectory(prefix="verify-oci-") as temporary:
            root = Path(temporary)
            layout = root / "image"
            layout.mkdir()
            (layout / "oci-layout").write_text('{"imageLayoutVersion":"1.0.0"}\n')
            (layout / "blobs").symlink_to(self.layout.resolve() / "blobs", target_is_directory=True)
            for child, (_, arch) in zip(children, platforms):
                self._verify_platform(stage, layout, child, arch)
        return top

    def _verify_platform(self, stage: "Stage", layout: Path, child: dict, arch: str) -> None:
        image = json.loads(_blob(layout, child).read_bytes())
        if child["mediaType"] != MANIFEST or image["schemaVersion"] != 2 or image["mediaType"] != MANIFEST:
            raise ValueError(f"expected an OCI image manifest for linux/{arch}")
        config = json.loads(_blob(layout, image["config"]).read_bytes())
        if (image["config"]["mediaType"], config["os"], config["architecture"]) != (CONFIG, "linux", arch):
            raise ValueError(f"OCI platform config differs for linux/{arch}")
        if config["config"]["Entrypoint"] != ["/usr/local/bin/bktec"] or config["config"].get("Cmd"):
            raise ValueError(f"OCI legacy entrypoint differs for linux/{arch}")
        for layer in image["layers"]:
            _blob(layout, layer)

        context = f"image/linux_{arch}"
        for name in ("Dockerfile", "bktec", "bktest"):
            asset = stage.assets[f"{context}/{name}"]
            if digest(asset.path) != asset.sha256:
                raise ValueError(f"staged OCI asset differs: {context}/{name}")

        # Unpack the merged filesystem, not just the COPY layer: a later overlay
        # could obscure either path, alter its mode or redirect it with a link.
        (layout / "index.json").write_text(json.dumps({
            "schemaVersion": 2, "manifests": [dict(child, annotations={"org.opencontainers.image.ref.name": "check"})],
        }))
        bundle = layout.parent / arch
        _run("umoci", "unpack", "--rootless", "--image", f"{layout}:check", str(bundle))
        rootfs = bundle / "rootfs"
        expected = stage.assets[f"{context}/bktec"].sha256
        for name in ("bktec", "bktest"):
            path = rootfs / "usr/local/bin" / name
            if (stage.assets[f"{context}/{name}"].sha256 != expected or not path.resolve().is_relative_to(rootfs)
                    or not path.is_file() or not path.stat().st_mode & 0o111 or digest(path) != expected):
                raise ValueError(f"OCI payload differs from stage: linux/{arch} /usr/local/bin/{name}")

    def _remote_digest(self, ref: str) -> str | None:
        local = self._loopback()
        options = ("--tls-verify=false", "--no-creds") if local else ()
        try:
            data = _run("skopeo", "inspect", "--raw", *options, f"docker://{ref}", loopback=local)
        except OSError as error:
            # Treat only the registry's missing-manifest response as absence,
            # not auth, namespace, TLS or connectivity failures.
            if re.search(r': manifest unknown"?$', str(error)):
                return None
            raise
        return "sha256:" + hashlib.sha256(data).hexdigest()

    def _present(self, ref: str, top: dict) -> bool:
        actual = self._remote_digest(ref)
        if actual is None:
            return False
        if actual != self.index_digest:
            raise ValueError(f"OCI version already exists with a different digest: {ref}")
        # A matching tag does not prove that all referenced blobs can be pulled.
        # Read back the exact digest, not the tag, into an empty OCI layout.
        pinned = f"{self.name}@{self.index_digest}"
        local = self._loopback()
        options = ("--src-tls-verify=false", "--src-no-creds") if local else ()
        with TemporaryDirectory(prefix="readback-oci-") as temporary:
            layout = Path(temporary)
            try:
                _run("skopeo", "copy", "--all", "--preserve-digests", "--remove-signatures",
                     "--dest-oci-accept-uncompressed-layers",
                     *options, f"docker://{pinned}", f"oci:{layout}", loopback=local)
                index = json.loads(_blob(layout, top).read_bytes())
                for child in index["manifests"]:
                    image = json.loads(_blob(layout, child).read_bytes())
                    for descriptor in (image["config"], *image["layers"]):
                        _blob(layout, descriptor)
            except OSError as error:
                raise OSError(f"cannot read back OCI content for {ref}: {error}") from error
        return True

    def preflight(self, stage: "Stage") -> tuple[str, ...]:
        ref, top = self._inventory(stage)
        return () if self._present(ref, top) else (ref,)

    def publish_missing(self, stage: "Stage", missing: tuple[str, ...]) -> None:
        if not missing:
            return
        if not self._loopback():
            raise ValueError("OCI writes are restricted to loopback rehearsal")
        ref, top = self._inventory(stage)
        if missing != (ref,):
            raise ValueError("OCI publication may only write the requested exact-version index")
        if self._present(ref, top):
            return
        _run("skopeo", "copy", "--all", "--preserve-digests", "--dest-tls-verify=false", "--dest-no-creds",
             f"oci:{self.layout.resolve()}", f"docker://{ref}", loopback=True)
        if not self._present(ref, top):
            raise OSError(f"OCI version absent after copy: {ref}")
