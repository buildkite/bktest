@testable import Core
import OpenTelemetryApi
import OpenTelemetrySdk
import XCTest

final class SynchronousExecutionSpanProcessorTests: XCTestCase {
  func testBacksOffFailedExportsAndResetsAfterSuccess() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [
      .failure(.httpStatus(503)), .failure(.httpStatus(503)), .failure(.httpStatus(503)), .failure(.httpStatus(503)),
      .failure(.httpStatus(503)), .success(()), .failure(.httpStatus(503)), .success(()),
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
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))])
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
      .failure(.httpStatus(403)),
      .failure(.httpStatus(403)),
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
    XCTAssertEqual(messages.withValue { $0.filter { $0.hasPrefix("[BuildkiteTestCollector] warning:") } }, [
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 403): one. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 3 test.execution span(s) so far this run; those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  func testDropsExecutionsStillUnsentAtFlushAndStartsAFreshCountPerRun() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [
      .failure(.transportError),
      .failure(.httpStatus(503)),
      .failure(.httpStatus(401)),
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
    XCTAssertEqual(messages.withValue { $0.filter { !$0.hasPrefix("[BuildkiteTestCollector] debug:") } }, [
      "[BuildkiteTestCollector] error: OpenTelemetry export failed for one (socket_error); 1 execution(s) remain queued in this process",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 2 test.execution span(s) (unsent at flush, last OTLP failure: 503): one … two. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 401): three. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  func testDeliversABacklogInBoundedBatchesAfterATransientFailure() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))]) { spans in
      spans.count > SynchronousExecutionSpanProcessor.maxBatchSpanCount ? .failure(.httpStatus(413)) : .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now },
      sleep: { clock.now += $0 }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    for index in 1...10_000 {
      self.endSpan("test\(index)", with: tracer)
    }
    XCTAssertEqual(exporter.exports.count, 1)
    processor.forceFlush(timeout: 30)
    logger.waitForLogs()

    XCTAssertEqual(exporter.exports.count, 85)
    XCTAssertEqual(exporter.exports.map(\.count).max(), 120)
    XCTAssertEqual(exporter.exports.dropFirst().flatMap { $0.map(\.name) }, (1...10_000).map { "test\($0)" })
    XCTAssertEqual(messages.withValue { $0.filter { !$0.hasPrefix("[BuildkiteTestCollector] debug:") } }, [
      "[BuildkiteTestCollector] error: OpenTelemetry export failed for test1 (503); 1 execution(s) remain queued in this process",
    ])
  }

  func testSplitsBatchesByEncodedSizeAndDropsOnlyTheBatchTheReceiverRejects() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))]) { spans in
      let payloadSize = spans.reduce(0) { size, span in
        size + span.attributes.values.reduce(0) { $0 + $1.description.utf8.count }
      }
      return payloadSize > 600 * 1024 ? .failure(.httpStatus(413)) : .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now },
      sleep: { clock.now += $0 }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    self.endSpan("one", with: tracer)
    tracer.spanBuilder(spanName: "huge")
      .setNoParent()
      .setAttribute(key: "payload", value: String(repeating: "x", count: 700 * 1024))
      .startSpan()
      .end()
    self.endSpan("two", with: tracer)
    self.endSpan("three", with: tracer)
    processor.forceFlush(timeout: 30)
    logger.waitForLogs()

    XCTAssertEqual(
      exporter.exports.map { $0.map(\.name) },
      [["one"], ["one"], ["huge"], ["two", "three"]]
    )
    XCTAssertEqual(messages.withValue { $0.filter { $0.hasPrefix("[BuildkiteTestCollector] warning:") } }, [
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 1 test.execution span(s) (export-failure, last OTLP failure: 413): huge. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  func testStopsExportingAtTheFlushDeadlineAndDropsTheRemainder() {
    let clock = TestClock()
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))]) { _ in
      clock.now += 8
      return .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in
      messages.withValue { $0.append(message) }
    })
    let processor = SynchronousExecutionSpanProcessor(
      exporter: exporter,
      logger: logger,
      now: { clock.now },
      sleep: { clock.now += $0 }
    )
    let tracer = TracerProviderSdk(spanProcessors: [processor]).get(
      instrumentationName: "SynchronousExecutionSpanProcessorTests"
    )

    for index in 1...600 {
      self.endSpan("test\(index)", with: tracer)
    }
    clock.now = 10
    processor.forceFlush(timeout: 30)
    logger.waitForLogs()

    XCTAssertEqual(exporter.exports.map(\.count), [1, 120, 120, 120, 120])
    XCTAssertEqual(exporter.timeouts, [nil, 30, 22, 14, 6])
    XCTAssertEqual(clock.now, 42)
    XCTAssertEqual(messages.withValue { $0.filter { $0.hasPrefix("[BuildkiteTestCollector] warning:") } }, [
      "[BuildkiteTestCollector] warning: TEST RESULTS MISSING: OpenTelemetry dropped 120 test.execution span(s) (unsent at flush): test481 … test600. OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.",
    ])
  }

  private func endSpan(_ name: String, with tracer: any Tracer) {
    tracer.spanBuilder(spanName: name).setNoParent().startSpan().end()
  }
}

private final class TestClock {
  var now: TimeInterval = 0
}

/// Answers each export with the next scripted result, then with `handler`.
private final class SequencedExporter: FailureReportingSpanExporter, @unchecked Sendable {
  private var results: [Result<Void, SpanExportFailure>]
  private let handler: ([SpanData]) -> Result<Void, SpanExportFailure>
  private(set) var lastFailure: SpanExportFailure?
  var exports = [[SpanData]]()
  var timeouts = [TimeInterval?]()

  init(
    results: [Result<Void, SpanExportFailure>],
    then handler: @escaping ([SpanData]) -> Result<Void, SpanExportFailure> = { _ in .success(()) }
  ) {
    self.results = results
    self.handler = handler
  }

  func export(spans: [SpanData], explicitTimeout: TimeInterval?) -> SpanExporterResultCode {
    self.exports.append(spans)
    self.timeouts.append(explicitTimeout)
    switch self.results.isEmpty ? self.handler(spans) : self.results.removeFirst() {
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
