"""Local integration checks for exact-version OCI publication."""

from dataclasses import replace
from io import BytesIO
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
from tempfile import TemporaryDirectory
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from publish_oci import EXISTING_REPOSITORIES, OCIDestination


class OCIPublicationTest(unittest.TestCase):
    def setUp(self):
        temporary = TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.stage = SimpleNamespace(version="3.7.0", archive_sha256="a" * 64, assets={})

    def layout(self, *, arm64_bktest=None):
        folder = self.root / ("different" if arm64_bktest else "image")
        blobs = folder / "blobs/sha256"
        blobs.mkdir(parents=True)

        def blob(kind, data):
            if not isinstance(data, bytes):
                data = json.dumps(data).encode()
            sha = hashlib.sha256(data).hexdigest()
            (blobs / sha).write_bytes(data)
            return {"mediaType": f"application/vnd.oci.image.{kind}", "digest": f"sha256:{sha}", "size": len(data)}

        children = []
        for arch in ("amd64", "arm64"):
            binary = f"payload for {arch}".encode()
            for name, data in [("Dockerfile", b"FROM scratch\n"), ("bktec", binary), ("bktest", binary)]:
                path = self.root / f"{arch}-{name}"
                path.write_bytes(data)
                self.stage.assets[f"image/linux_{arch}/{name}"] = SimpleNamespace(
                    path=path, sha256=hashlib.sha256(data).hexdigest(),
                )
            buffer = BytesIO()
            with tarfile.open(fileobj=buffer, mode="w") as layer:
                for name in ("bktec", "bktest"):
                    data = arm64_bktest if arch == "arm64" and name == "bktest" and arm64_bktest else binary
                    entry = tarfile.TarInfo(f"usr/local/bin/{name}")
                    entry.mode, entry.size = 0o755, len(data)
                    layer.addfile(entry, BytesIO(data))
            filesystem = blob("layer.v1.tar", buffer.getvalue())
            config = blob("config.v1+json", {
                "os": "linux", "architecture": arch, "config": {"Entrypoint": ["/usr/local/bin/bktec"]},
                "rootfs": {"type": "layers", "diff_ids": [filesystem["digest"]]},
            })
            manifest = blob("manifest.v1+json", {
                "schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "config": config, "layers": [filesystem],
            })
            children.append(dict(manifest, platform={"os": "linux", "architecture": arch}))
        index = blob("index.v1+json", {
            "schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": children,
        })
        (folder / "oci-layout").write_text('{"imageLayoutVersion":"1.0.0"}')
        (folder / "index.json").write_text(json.dumps({"schemaVersion": 2, "manifests": [index]}))
        return folder, index["digest"]

    def test_rejects_new_names_prerelease_and_mismatched_archive_before_inspection(self):
        destination = OCIDestination("docker.io/buildkite/bktest", self.root / "absent", "", "a" * 64)
        with self.assertRaisesRegex(ValueError, "not an existing OCI destination"):
            destination.preflight(self.stage)
        destination = replace(destination, name="docker.io/buildkite/test-engine-client")
        with self.assertRaisesRegex(ValueError, "writes are restricted to loopback"):
            destination.publish_missing(self.stage, (destination.name + ":v3.7.0",))
        with self.assertRaisesRegex(ValueError, "does not match the stage archive"):
            replace(destination, archive_sha256="b" * 64).preflight(self.stage)
        self.stage.version = "3.7.0-rc.1"
        with self.assertRaisesRegex(ValueError, "requires an exact stable CLI version"):
            destination.preflight(self.stage)

    def test_registry_specific_absence_does_not_adopt_ambiguous_failures(self):
        _, ecr, packages = EXISTING_REPOSITORIES
        tag = "v3.7.0"
        missing = OSError(r'skopeo inspect failed: level=fatal msg="reading manifest: StatusCode: 404, \"\""')
        for name, responses, expected in (
            (ecr, [OSError('reading manifest: manifest unknown: Requested image not found"')], None),
            (ecr, [OSError("reading manifest: name unknown")], OSError),
            (packages, [missing, json.dumps({"Repository": packages, "Tags": ["v3.5.0"]}).encode()], None),
            (packages, [missing, json.dumps({"Repository": packages, "Tags": [tag]}).encode()], ValueError),
            (packages, [missing, json.dumps({"Repository": ecr, "Tags": ["v3.5.0"]}).encode()], ValueError),
            (packages, [missing, OSError("error listing repository tags: 404")], OSError),
            (packages, [OSError("reading manifest: unauthorized")], OSError),
        ):
            with self.subTest(name=name, responses=responses), patch("publish_oci._run", side_effect=responses) as run:
                destination = OCIDestination(name, self.root, "", self.stage.archive_sha256)
                if expected is None:
                    self.assertIsNone(destination._remote_digest(f"{name}:{tag}"))
                else:
                    with self.assertRaises(expected):
                        destination._remote_digest(f"{name}:{tag}")
                if len(responses) == 2:
                    run.assert_called_with("skopeo", "list-tags", f"docker://{name}")

    @unittest.skipUnless(all(shutil.which(tool) for tool in ("skopeo", "umoci", "docker-registry")),
                         "requires skopeo, umoci and a local Distribution registry")
    def test_push_readback_retry_and_conflict_never_write_latest(self):
        config = self.root / "registry.json"
        config.write_text(json.dumps({
            "version": "0.1", "storage": {"filesystem": {"rootdirectory": str(self.root / "registry")}},
            "http": {"addr": "127.0.0.1:0"},
        }))
        log = self.root / "registry.log"
        with log.open("wb") as output:
            registry = subprocess.Popen(["docker-registry", "serve", str(config)], stdout=output,
                                        stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline and registry.poll() is None:
                    if match := re.search(r"listening on (127\.0\.0\.1:[0-9]+)", log.read_text()):
                        name = f"{match[1]}/buildkite/test-engine-client-docker/test-engine-client"
                        break
                    time.sleep(0.05)
                else:
                    self.fail("disposable Distribution registry did not start")
                self.exercise_registry(name)
                version_puts = [line for line in log.read_text().splitlines() if
                                'http.request.method=PUT' in line and '/manifests/v3.7.0' in line]
                self.assertEqual(len(version_puts), 1, "an identical retry must not PUT the version tag again")
            finally:
                registry.terminate()
                registry.wait(timeout=5)

    def exercise_registry(self, name):
        image, sha = self.layout()
        conflict, other_sha = self.layout(arm64_bktest=b"wrong arm64 CLI")
        destination = OCIDestination(name, image, sha, self.stage.archive_sha256)
        ref, = destination.preflight(self.stage)
        self.assertEqual(ref, f"{name}:v3.7.0")
        with self.assertRaisesRegex(ValueError, "only write.*exact-version"):
            destination.publish_missing(self.stage, (f"{name}:latest",))
        with self.assertRaisesRegex(ValueError, "differs from the approved index"):
            replace(destination, index_digest=other_sha).preflight(self.stage)
        with self.assertRaisesRegex(ValueError, "OCI payload differs from stage: linux/arm64.*bktest"):
            replace(destination, layout=conflict, index_digest=other_sha).preflight(self.stage)
        approved_blob = image / "blobs/sha256" / sha.removeprefix("sha256:")
        original = approved_blob.read_bytes()
        approved_blob.write_bytes(b"x" * len(original))
        with self.assertRaisesRegex(ValueError, "OCI blob differs"):
            destination.preflight(self.stage)
        approved_blob.write_bytes(original)

        latest = f"docker://{name}:latest"
        tagged = f"docker://{ref}"

        def seed(source, target):
            subprocess.run(["skopeo", "copy", "--all", "--preserve-digests", "--dest-tls-verify=false",
                            f"oci:{source}", target], check=True, capture_output=True)

        def read(target):
            return subprocess.check_output(["skopeo", "inspect", "--raw", "--tls-verify=false", target])

        seed(conflict, latest)
        latest_before = read(latest)
        destination.publish_missing(self.stage, (ref,))
        raw = read(tagged)
        self.assertEqual(f"sha256:{hashlib.sha256(raw).hexdigest()}", sha)
        self.assertEqual(destination.preflight(self.stage), ())
        destination.publish_missing(self.stage, (ref,))  # Retry a stale preflight result.
        self.assertEqual(read(tagged), raw)
        self.assertEqual(read(latest), latest_before)

        # Keep the matching index and manifests visible, but remove a referenced
        # layer from the registry's content store (not the approved layout).
        header = json.loads((image / "index.json").read_bytes())["manifests"][0]
        blobs = image / "blobs/sha256"
        index = json.loads((blobs / header["digest"].removeprefix("sha256:")).read_bytes())
        child = index["manifests"][0]["digest"].removeprefix("sha256:")
        manifest = json.loads((blobs / child).read_bytes())
        for descriptor in (manifest["config"], *manifest["layers"]):
            sha256 = descriptor["digest"].removeprefix("sha256:")
            stored = self.root / "registry/docker/registry/v2/blobs/sha256" / sha256[:2] / sha256 / "data"
            saved = stored.read_bytes()
            stored.unlink()
            try:
                self.assertEqual(read(tagged), raw)
                self.assertTrue(read(f"docker://{name}@sha256:{child}"))
                with self.assertRaisesRegex(OSError, "cannot read back OCI content"):
                    destination.preflight(self.stage)
            finally:
                stored.write_bytes(saved)

        self.stage.version = "3.7.1"
        next_ref, = destination.preflight(self.stage)
        seed(conflict, f"docker://{next_ref}")  # Another writer after preflight.
        with self.assertRaisesRegex(ValueError, "already exists with a different digest"):
            destination.publish_missing(self.stage, (next_ref,))
        self.assertEqual(read(f"docker://{next_ref}"), latest_before)
        self.assertEqual(read(latest), latest_before)
