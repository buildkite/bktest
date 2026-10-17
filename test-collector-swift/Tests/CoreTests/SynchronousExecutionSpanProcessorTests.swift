@testable import Core
import OpenTelemetryApi
import OpenTelemetrySdk
import XCTest

final class SynchronousExecutionSpanProcessorTests: XCTestCase {
  func testBacksOffFailedExportsAndResetsAfterSuccess() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [
      .failure(.transient), .failure(.transient), .failure(.transient), .failure(.transient),
      .failure(.transient), .success(()), .failure(.transient), .success(()),
    ])
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: nil,
      now: { clock.now }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    self.endSpan("one", with: tracer)
    self.endSpan("two", with: tracer)
    clock.now = 9
    self.endSpan("three", with: tracer)
    XCTAssertEqual(exporter.exports.count, 1)

    clock.now = 10
    self.endSpan("four", with: tracer)
    clock.now = 29
    self.endSpan("five", with: tracer)
    XCTAssertEqual(exporter.exports.count, 2)

    clock.now = 30
    self.endSpan("six", with: tracer)
    clock.now = 59
    self.endSpan("seven", with: tracer)
    XCTAssertEqual(exporter.exports.count, 3)

    clock.now = 60
    self.endSpan("eight", with: tracer)
    clock.now = 119
    self.endSpan("nine", with: tracer)
    XCTAssertEqual(exporter.exports.count, 4)

    clock.now = 120
    self.endSpan("ten", with: tracer)
    clock.now = 179
    self.endSpan("eleven", with: tracer)
    XCTAssertEqual(exporter.exports.count, 5)

    clock.now = 180
    self.endSpan("twelve", with: tracer)
    self.endSpan("thirteen", with: tracer)
    clock.now = 189
    self.endSpan("fourteen", with: tracer)
    XCTAssertEqual(exporter.exports.count, 7)

    clock.now = 190
    self.endSpan("fifteen", with: tracer)

    XCTAssertEqual(exporter.exports.count, 8)
    XCTAssertEqual(exporter.exports.map(\.count), [1, 4, 6, 8, 10, 12, 1, 3])
  }

  func testLogsExecutionsQueuedDuringBackoff() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [.failure(.transient)])
    let messages = LockIsolated([String]())
    let logger = Logger(logLevel: .debug, printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    self.endSpan("one", with: tracer)
    self.endSpan("two", with: tracer)
    logger.waitForLogs()

    XCTAssertTrue(messages.withValue { messages in
      messages.contains(
        "[BuildkiteTestCollector] debug: Queued OpenTelemetry test execution during export backoff: two (2 pending)"
      )
    })
  }

  func testDropsExecutionsRejectedByTheReceiverAndReportsTotalsAtFlush() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [
      .failure(SpanExportFailure(reason: "403", isRetryable: false)),
      .failure(SpanExportFailure(reason: "403", isRetryable: false)),
      .success(()),
    ])
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    self.endSpan("one", with: tracer)
    clock.now = 5
    self.endSpan("two", with: tracer)
    clock.now = 10
    self.endSpan("three", with: tracer)
    clock.now = 30
    self.endSpan("four", with: tracer)
    processor.forceFlush(timeout: nil)
    logger.waitForLogs()

    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one"], ["two", "three"], ["four"]])
    XCTAssertEqual(messages.withValue { $0.filter { $0.contains("warning") } }, [
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 403). OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 3 test.execution span(s) so far this run; those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  func testDropsExecutionsStillUnsentAtFlushAndStartsAFreshCountPerRun() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [
      .failure(SpanExportFailure(reason: "socket_error", isRetryable: true)),
      .failure(SpanExportFailure(reason: "503", isRetryable: true)),
      .failure(SpanExportFailure(reason: "401", isRetryable: false)),
    ])
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    self.endSpan("one", with: tracer)
    self.endSpan("two", with: tracer)
    processor.forceFlush(timeout: nil)
    clock.now = 60
    self.endSpan("three", with: tracer)
    processor.forceFlush(timeout: nil)
    logger.waitForLogs()

    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one"], ["one", "two"], ["three"]])
    XCTAssertEqual(messages.withValue { $0.filter { !$0.contains("debug") } }, [
      "[BuildkiteTestCollector] error: OpenTelemetry export failed for one (socket_error); the execution remains queued in this process",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 2 test.execution span(s) (export-failure, last OTLP failure: 503). OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 401). OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  private func endSpan(_ name: String, with tracer: any Tracer) {
    tracer.spanBuilder(spanName: name).setNoParent().startSpan().end()
  }
}

private final class TestClock {
  var now: TimeInterval = 0
}

private extension SpanExportFailure {
  static let transient = SpanExportFailure(reason: "503", isRetryable: true)
}

private final class SequencedExporter: FailureReportingSpanExporter, @unchecked Sendable {
  private var results: [Result<Void, SpanExportFailure>]
  private(set) var lastFailure: SpanExportFailure?
  var exports = [[SpanData]]()

  init(results: [Result<Void, SpanExportFailure>]) {
    self.results = results
  }

  func export(spans: [SpanData], explicitTimeout: TimeInterval?) -> SpanExporterResultCode {
    self.exports.append(spans)
    switch self.results.removeFirst() {
    case .success:
      self.lastFailure = nil
      return .success
    case let .failure(failure):
      self.lastFailure = failure
      return .failure
    }
  }

  func flush(explicitTimeout: TimeInterval?) -> SpanExporterResultCode {
    .success
  }

  func shutdown(explicitTimeout: TimeInterval?) {}
}
