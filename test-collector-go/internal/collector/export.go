package collector

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var httpStatusInError = regexp.MustCompile(`: (\d{3}) [A-Za-z]`)

// newExporter builds the OTLP/HTTP exporter for one span stream with every
// setting passed explicitly, so the OTEL_EXPORTER_OTLP_* variables bktec and
// vendor exporters set for other destinations are never consulted.
func newExporter(ctx context.Context, endpoint *url.URL, token, runKey, stream string) (*otlptrace.Exporter, error) {
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
	client := &http.Client{Transport: transport}
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
	lastFailure string
	reported    int
}

func (s *streamStats) offer(n int) {
	s.mu.Lock()
	s.offered += n
	s.mu.Unlock()
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
	switch {
	case s.failed > 0 && notExported > 0:
		r.reason = "export-failure, not-exported"
	case s.failed > 0:
		r.reason = "export-failure"
	case notExported > 0:
		r.reason = "not-exported"
	case s.startFailed > 0:
		r.reason = "could not start span"
	}
	if s.failed > 0 && s.lastFailure != "" {
		r.reason += ", last OTLP failure: " + s.lastFailure
	}
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
// cause, and keeps a panicking exporter from taking the batch worker down.
type countingExporter struct {
	sdktrace.SpanExporter
	stats *streamStats
}

func (e countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("exporter panicked: %v", r)
			WarnOnce("export-panic:"+fmt.Sprintf("%T", r), "Could not export OpenTelemetry spans: %v. Further %T export failures will not be reported.\n%s", r, r, debug.Stack())
		}
		e.stats.mu.Lock()
		defer e.stats.mu.Unlock()
		if err != nil {
			e.stats.failed += len(spans)
			e.stats.lastFailure = failureCause(err)
		} else {
			e.stats.succeeded += len(spans)
			e.stats.lastFailure = ""
		}
	}()
	return e.SpanExporter.ExportSpans(ctx, spans)
}

// failureCause reduces an export error to the HTTP status when there is one,
// otherwise to the first line of the error.
func failureCause(err error) string {
	msg := err.Error()
	if m := httpStatusInError.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
