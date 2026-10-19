package collector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// newExporter builds the OTLP/HTTP exporter for one span stream with every
// setting passed explicitly, so the OTEL_EXPORTER_OTLP_* variables bktec and
// vendor exporters set for other destinations are never consulted.
func newExporter(ctx context.Context, endpoint *url.URL, token, runKey, stream string, stats *streamStats) (*otlptrace.Exporter, error) {
	// A transport of our own, not a clone of http.DefaultTransport: a suite
	// that relaxes TLS verification or swaps in a fake transport for its own
	// tests must not change how results reach Buildkite.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Transport: statusRecorder{RoundTripper: transport, stats: stats}}
	return otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint.String()),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization":  `Token token="` + token + `"`,
			runKeyHeader:     runKey,
			spanStreamHeader: stream,
		}),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 5 * time.Second,
			MaxInterval:     30 * time.Second,
			MaxElapsedTime:  time.Minute,
		}),
		otlptracehttp.WithHTTPClient(client),
	)
}

// streamStats counts what one stream's batch processor was offered and what
// its exporter did with it. The SDK exposes no drop callback, so loss is
// derived once a drain has completed: offered − succeeded − failed never
// reached the exporter (queue overflow or shutdown cut-off), failed is what
// the exporter could not deliver.
type streamStats struct {
	mu          sync.Mutex
	offered     int
	succeeded   int
	failed      int
	startFailed int
	// lastStatus is the HTTP status of the current export's last response,
	// 0 until it gets one.
	lastStatus  int
	lastFailure string
	reported    int
}

func (s *streamStats) offer(n int) {
	s.mu.Lock()
	s.offered += n
	s.mu.Unlock()
}

// inFlight is the number of spans offered to the processor whose export has
// not completed: queued, batched or on the wire.
func (s *streamStats) inFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offered - s.succeeded - s.failed
}

// startFailure counts a span that could not be started. The caller warns
// about it immediately, so it is already reported.
func (s *streamStats) startFailure() {
	s.mu.Lock()
	s.startFailed++
	s.reported++
	s.mu.Unlock()
}

type dropReport struct {
	dropped int
	reason  string
	pending int
}

// unreported computes the loss visible so far and reports whether it has
// grown since the last warning, marking it reported if so. drained says the
// last flush or shutdown completed within its deadline; otherwise spans
// whose export has not finished may still be in flight and are counted as
// pending rather than dropped.
func (s *streamStats) unreported(drained bool) (dropReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notExported := s.offered - s.succeeded - s.failed
	var r dropReport
	if !drained {
		r.pending = notExported
		notExported = 0
	}
	r.dropped = notExported + s.failed + s.startFailed
	// Each reason gets its own count, and the last OTLP failure is named
	// only after the export failures it caused, last so it reads as theirs.
	var reasons []string
	if s.startFailed > 0 {
		reasons = append(reasons, fmt.Sprintf("could not start span: %d", s.startFailed))
	}
	if notExported > 0 {
		reasons = append(reasons, fmt.Sprintf("not-exported: %d", notExported))
	}
	if s.failed > 0 {
		failure := fmt.Sprintf("export-failure: %d", s.failed)
		if s.lastFailure != "" {
			failure += ", last OTLP failure: " + s.lastFailure
		}
		reasons = append(reasons, failure)
	}
	r.reason = strings.Join(reasons, ", ")
	if r.dropped <= s.reported {
		return r, false
	}
	s.reported = r.dropped
	return r, true
}

// countingProcessor records how many spans reached the batch processor.
type countingProcessor struct {
	sdktrace.SpanProcessor
	stats *streamStats
}

func (p countingProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.stats.offer(1)
	p.SpanProcessor.OnEnd(s)
}

// countingExporter records per-span export outcomes and the last failure
// cause, keeps a panicking exporter from taking the batch worker down, and
// redacts the request headers from export errors: the SDK logs the error it
// gets back, and a server may echo the headers it rejected.
type countingExporter struct {
	sdktrace.SpanExporter
	stats   *streamStats
	secrets []string
}

func (e countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("exporter panicked: %v", r)
			WarnOnce("export-panic:"+fmt.Sprintf("%T", r), "Could not export OpenTelemetry spans: %v. Further %T export failures will not be reported.\n%s", r, r, debug.Stack())
		}
		if err != nil {
			err = redact(err, e.secrets)
		}
		e.stats.mu.Lock()
		defer e.stats.mu.Unlock()
		// A later successful batch leaves lastFailure alone: it names the
		// cause of the spans already lost, which are reported at the end.
		if err != nil {
			e.stats.failed += len(spans)
			e.stats.lastFailure = failureCause(err, e.stats.lastStatus)
		} else {
			e.stats.succeeded += len(spans)
		}
	}()
	e.stats.mu.Lock()
	e.stats.lastStatus = 0
	e.stats.mu.Unlock()
	return e.SpanExporter.ExportSpans(ctx, spans)
}

// minRedactedLength is the shortest secret redact replaces.
const minRedactedLength = 8

// redact replaces each secret in the error's text with [REDACTED].
func redact(err error, secrets []string) error {
	msg := err.Error()
	redacted := msg
	for _, secret := range secrets {
		// A run key can be a single character; redacting "4" would rewrite
		// every status code and URL in the message.
		if len(secret) >= minRedactedLength {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

// statusRecorder records the status of each response on the stream's
// stats: the exporter's error for a status it retries (429, 502, 503, 504)
// carries the response body but not the status.
type statusRecorder struct {
	http.RoundTripper
	stats *streamStats
}

func (t statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	status := 0
	if err == nil {
		status = resp.StatusCode
	}
	t.stats.mu.Lock()
	t.stats.lastStatus = status
	t.stats.mu.Unlock()
	return resp, err
}

// failureCause reduces an export error to the HTTP status of the last
// response when that was an error status, otherwise to the first line of
// the error.
func failureCause(err error, lastStatus int) string {
	if lastStatus >= http.StatusMultipleChoices {
		return strconv.Itoa(lastStatus)
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
