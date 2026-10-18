"""Test harness for OTel mode: a loopback OTLP receiver and a subprocess pytest runner."""

import gzip
import os
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Dict, List, Mapping, Optional

from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest

DATA_DIR = Path(__file__).parent / "data"
SAMPLE_FILE = DATA_DIR / "test_sample_otel.py"


@dataclass(frozen=True)
class ReceivedSpan:
    """One span decoded from an OTLP request, with scalar attributes flattened."""

    name: str
    trace_id: bytes
    span_id: bytes
    parent_span_id: bytes
    start_time_unix_nano: int
    end_time_unix_nano: int
    status_code: int
    status_message: str
    attributes: Dict[str, object]
    events: List[Dict[str, object]]
    resource: Dict[str, object]


@dataclass(frozen=True)
class ReceivedRequest:
    """One HTTP request as the receiver saw it."""

    path: str
    headers: Dict[str, str]
    content_type: str
    encoding: Optional[str]
    spans: List[ReceivedSpan]
    status: int

    def header(self, name: str) -> Optional[str]:
        """Look a header up case-insensitively."""
        return self.headers.get(name.lower())


def _attribute_value(value) -> object:
    kind = value.WhichOneof("value")
    if kind is None:
        return None
    return getattr(value, kind)


def _attributes(attributes) -> Dict[str, object]:
    return {attribute.key: _attribute_value(attribute.value) for attribute in attributes}


def _decode_spans(body: bytes) -> List[ReceivedSpan]:
    request = ExportTraceServiceRequest()
    request.ParseFromString(body)
    spans = []
    for resource_spans in request.resource_spans:
        resource = _attributes(resource_spans.resource.attributes)
        for scope_spans in resource_spans.scope_spans:
            for span in scope_spans.spans:
                spans.append(ReceivedSpan(
                    name=span.name,
                    trace_id=span.trace_id,
                    span_id=span.span_id,
                    parent_span_id=span.parent_span_id,
                    start_time_unix_nano=span.start_time_unix_nano,
                    end_time_unix_nano=span.end_time_unix_nano,
                    status_code=span.status.code,
                    status_message=span.status.message,
                    attributes=_attributes(span.attributes),
                    events=[
                        {"name": event.name, **_attributes(event.attributes)}
                        for event in span.events
                    ],
                    resource=resource,
                ))
    return spans


@dataclass
class OTLPReceiver:
    """A loopback HTTP server recording every request it gets (traces or otherwise)."""

    requests: List[ReceivedRequest] = field(default_factory=list)
    next_statuses: List[int] = field(default_factory=list)
    first_request_delay: float = 0.0
    _server: Optional[HTTPServer] = field(default=None, init=False, repr=False, compare=False)

    def start(self) -> None:
        """Bind to an ephemeral port and serve on a daemon thread."""
        receiver = self

        class Handler(BaseHTTPRequestHandler):
            """Record the request, then answer with the next scripted status (default 200)."""

            def do_POST(self) -> None:  # pylint: disable=invalid-name
                """Handle an upload."""
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                receiver.record(self, body)

            def log_message(self, *args) -> None:
                """Silence the default access log."""

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self._server.serve_forever, daemon=True).start()

    def record(self, handler: BaseHTTPRequestHandler, body: bytes) -> None:
        """Decode and remember one request; reply with the scripted status."""
        if not self.requests and self.first_request_delay:
            time.sleep(self.first_request_delay)
        encoding = handler.headers.get("Content-Encoding")
        content_type = handler.headers.get("Content-Type", "")
        spans: List[ReceivedSpan] = []
        if handler.path.endswith("/v1/traces") and "protobuf" in content_type:
            spans = _decode_spans(gzip.decompress(body) if encoding == "gzip" else body)
        status = self.next_statuses.pop(0) if self.next_statuses else 200
        self.requests.append(ReceivedRequest(
            path=handler.path,
            headers={key.lower(): value for key, value in handler.headers.items()},
            content_type=content_type,
            encoding=encoding,
            spans=spans,
            status=status,
        ))
        handler.send_response(status)
        handler.send_header("Content-Type", "application/x-protobuf")
        handler.send_header("Content-Length", "0")
        handler.end_headers()

    def stop(self) -> None:
        """Shut the server down."""
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()

    @property
    def url(self) -> str:
        """Base URL of the receiver."""
        assert self._server is not None
        return f"http://127.0.0.1:{self._server.server_address[1]}"

    def trace_requests(self, stream: Optional[str] = None) -> List[ReceivedRequest]:
        """Requests to ``/v1/traces``, optionally only those for one span stream."""
        return [
            request for request in self.requests
            if request.path.endswith("/v1/traces")
            and (stream is None or request.header("Buildkite-Tests-Span-Stream") == stream)
        ]

    def spans(self, stream: str) -> List[ReceivedSpan]:
        """Every span received on ``stream`` across all requests."""
        return [span for request in self.trace_requests(stream) for span in request.spans]

    def upload_requests(self) -> List[ReceivedRequest]:
        """Requests to the JSON ``/v1/uploads`` endpoint."""
        return [request for request in self.requests if request.path.endswith("/v1/uploads")]


@dataclass(frozen=True)
class PytestRun:
    """The outcome of a subprocess pytest run."""

    returncode: int
    stdout: str
    stderr: str
    json_path: Path


def otel_env(receiver: OTLPReceiver, **overrides: Optional[str]) -> Dict[str, str]:
    """A clean environment that sends OTel traces and JSON uploads to ``receiver``."""
    env = {
        key: value for key, value in os.environ.items()
        if not key.startswith(("BUILDKITE", "OTEL_", "GITHUB_", "CIRCLE", "TRACEPARENT"))
    }
    env.update({
        "BUILDKITE_ANALYTICS_OTEL_ENABLED": "true",
        "BUILDKITE_TESTS_OTLP_TOKEN": "otlp-token",
        "BUILDKITE_ANALYTICS_TOKEN": "json-token",
        "BUILDKITE_ANALYTICS_OTLP_ENDPOINT": f"{receiver.url}/v1/traces",
        "BUILDKITE_ANALYTICS_API_URL": f"{receiver.url}/v1",
        "BUILDKITE_BUILD_ID": "build-1234",
        "BUILDKITE_BUILD_URL": "https://buildkite.com/acme/app/builds/1",
        "BUILDKITE_AGENT_ID": "agent-7",
    })
    for key, value in overrides.items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value
    return env


def run_pytest(
    tmp_path: Path,
    env: Mapping[str, str],
    *args: str,
    test_file: Path = SAMPLE_FILE,
    merge_output: bool = False,
) -> PytestRun:
    """Run ``test_file`` under the collector in a subprocess with ``env``.

    With ``merge_output`` the subprocess's stderr is interleaved into ``stdout``
    as a terminal would show it, and ``stderr`` is empty.
    """
    json_path = tmp_path / "results.json"
    cmd = [
        sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider",
        str(test_file), f"--json={json_path}", *args,
    ]
    result = subprocess.run(
        cmd, env=dict(env), text=True, check=False,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT if merge_output else subprocess.PIPE,
    )
    return PytestRun(result.returncode, result.stdout, result.stderr or "", json_path)
