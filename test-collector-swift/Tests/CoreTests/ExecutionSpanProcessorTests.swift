@testable import Core
import OpenTelemetryApi
import OpenTelemetryProtocolExporterCommon
import OpenTelemetrySdk
import XCTest

final class ExecutionSpanProcessorTests: XCTestCase {
  private let worker = DispatchQueue(label: "ExecutionSpanProcessorTests")

  func testRelayExportsEachExecutionBeforeReturning() {
    let exporter = SequencedExporter(results: [])
    let processor = self.processor(exporter, synchronous: true)
    let tracer = self.tracer(processor)
    for name in ["one", "two"] {
      // Unlike the direct-mode helper, do not join the worker after end().
      tracer.spanBuilder(spanName: name).setNoParent().startSpan().end()
      XCTAssertEqual(exporter.exports.last?.map(\.name), [name])
    }
    XCTAssertEqual(exporter.exports.map(\.count), [1, 1])
  }

  func testRelayQueuesDuringBackoffAndRecoversIn120SpanBatchesBeforeReturning() {
    let clock = LockIsolated(TimeInterval(0))
    let exporter = SequencedExporter(results: [.failure(.transportError)])
    let processor = self.processor(exporter, synchronous: true, now: { clock.value })
    let tracer = self.tracer(processor)
    for index in 1...122 {
      clock.setValue(index == 122 ? 9 : 0)
      tracer.spanBuilder(spanName: "test\(index)").setNoParent().startSpan().end()
      XCTAssertEqual(exporter.exports.count, 1)
    }
    clock.setValue(10)
    tracer.spanBuilder(spanName: "test123").setNoParent().startSpan().end()
    XCTAssertEqual(exporter.exports.map(\.count), [1, 120, 3])
    XCTAssertEqual(exporter.exports.dropFirst().flatMap { $0.map(\.name) }, (1...123).map { "test\($0)" })
  }

  func testRelay429WaitsOneSecondAndExportsBeforeReturningToTheTest() {
    let exporter = SequencedExporter(results: [.failure(.httpStatus(429))])
    let processor = self.processor(exporter, synchronous: true)
    let start = ProcessInfo.processInfo.systemUptime
    self.tracer(processor).spanBuilder(spanName: "backpressured").setNoParent().startSpan().end()
    let elapsed = ProcessInfo.processInfo.systemUptime - start
    XCTAssertGreaterThanOrEqual(elapsed, 0.95)
    XCTAssertLessThan(elapsed, 2.5)
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["backpressured"], ["backpressured"]])
    XCTAssertNil(exporter.lastFailure)
  }

  func testRelayBackpressureHonorsRetryAfterAndFallsBackToBackoffAfterTenSeconds() {
    for status in [429, 503] {
      for (header, delay, attempts) in [(nil, 1.0, 10), (2.0, 2.0, 5), (4.0, 4.0, 3), (99.0, 5.0, 2)] as [(Double?, Double, Int)] {
        let clock = LockIsolated(TimeInterval(0))
        let waits = LockIsolated([TimeInterval]())
        let exporter = SequencedExporter(results: Array(repeating: .failure(.httpStatus(status)), count: attempts))
        exporter.retryAfter = header
        let processor = self.processor(exporter, synchronous: true, now: { clock.value }, sleep: { delay in
          waits.withValue { $0.append(delay) }
          clock.withValue { $0 += delay }
        })
        let tracer = self.tracer(processor)
        tracer.spanBuilder(spanName: "one").setNoParent().startSpan().end()
        XCTAssertEqual(waits.value, header == 4 ? [4, 4, 2] : Array(repeating: delay, count: attempts))
        XCTAssertEqual(clock.value, 10)
        XCTAssertEqual(exporter.exports.count, attempts)
        // The exhausted handoff falls back to the normal 10 s outage backoff.
        tracer.spanBuilder(spanName: "two").setNoParent().startSpan().end()
        clock.setValue(19)
        tracer.spanBuilder(spanName: "three").setNoParent().startSpan().end()
        XCTAssertEqual(exporter.exports.count, attempts)
        clock.setValue(20)
        tracer.spanBuilder(spanName: "four").setNoParent().startSpan().end()
        XCTAssertEqual(exporter.exports.last?.map(\.name), ["one", "two", "three", "four"])
        XCTAssertNil(exporter.lastFailure)
      }
    }
  }

  func testRelayHonorsBatchAndQueueOverridesAndDrainsAFullQueueAfterBackoff() {
    let clock = LockIsolated(TimeInterval(0))
    let exporter = SequencedExporter(results: [.failure(.transportError)])
    let processor = self.processor(exporter, batch: "2", queue: "3", synchronous: true, now: { clock.value })
    let tracer = self.tracer(processor)
    for name in ["one", "two", "three", "dropped"] {
      tracer.spanBuilder(spanName: name).setNoParent().startSpan().end()
    }
    XCTAssertEqual(exporter.exports.map(\.count), [1])
    clock.setValue(10)
    tracer.spanBuilder(spanName: "alsoDropped").setNoParent().startSpan().end()
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one"], ["one", "two"], ["three"]])
    tracer.spanBuilder(spanName: "afterRecovery").setNoParent().startSpan().end()
    XCTAssertEqual(exporter.exports.last?.map(\.name), ["afterRecovery"])
  }

  func testConcurrentTestsAndFlushesExportEveryExecutionExactlyOnceInBothModes() {
    for synchronous in [false, true] {
      let exporter = SequencedExporter(results: [])
      let processor = self.processor(exporter, batch: "7", synchronous: synchronous)
      let tracer = self.tracer(processor)
      let finished = DispatchGroup()
      for producer in 0..<4 {
        DispatchQueue.global().async(group: finished) {
          for index in 0..<200 {
            tracer.spanBuilder(spanName: "\(producer)-\(index)").setNoParent().startSpan().end()
          }
        }
      }
      DispatchQueue.global().async(group: finished) {
        for _ in 0..<20 { processor.forceFlush(timeout: 5) }
      }
      XCTAssertEqual(finished.wait(timeout: .now() + 10), .success)
      processor.shutdown(explicitTimeout: 5)
      let names = exporter.exports.flatMap { $0.map(\.name) }
      XCTAssertEqual(names.count, 800)
      XCTAssertEqual(Set(names), Set((0..<4).flatMap { producer in
        (0..<200).map { "\(producer)-\($0)" }
      }))
      XCTAssertTrue(exporter.exports.allSatisfy { $0.count <= 7 })
    }
  }

  func testSendsAt120SpansWithoutWaitingForTheTimer() {
    let exporter = SequencedExporter(results: [])
    let processor = self.processor(exporter)
    let tracer = self.tracer(processor)
    for index in 1...119 { self.endSpan("test\(index)", with: tracer) }
    XCTAssertTrue(exporter.exports.isEmpty)
    self.endSpan("test120", with: tracer)
    XCTAssertEqual(exporter.exports.map(\.count), [120])
    XCTAssertEqual(exporter.exports[0].map(\.name), (1...120).map { "test\($0)" })
  }

  func testSendsWithinOneSecondOfFirstSpanWithoutAnotherTestOrFlush() {
    let sent = expectation(description: "timer export")
    let start = ProcessInfo.processInfo.systemUptime
    let exporter = SequencedExporter(results: []) { _ in
      // Allow scheduler jitter, but not another whole timer interval.
      XCTAssertLessThan(ProcessInfo.processInfo.systemUptime - start, 1.3)
      sent.fulfill()
      return .success(())
    }
    let processor = self.processor(exporter)
    let tracer = self.tracer(processor)
    self.endSpan("first", with: tracer)
    Thread.sleep(forTimeInterval: 0.65)
    self.endSpan("second", with: tracer)
    wait(for: [sent], timeout: 1)
    self.worker.sync {}
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["first", "second"]])
  }

  func testSlowExportDoesNotBlockTestThreadsAndQueueIsBounded() {
    let started = DispatchSemaphore(value: 0)
    let release = DispatchSemaphore(value: 0)
    let exporter = SequencedExporter(results: []) { _ in
      started.signal()
      _ = release.wait(timeout: .now() + 5)
      return .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    let processor = self.processor(exporter, batch: "2", queue: "3", logger: logger)
    let tracer = self.tracer(processor)
    self.endSpan("one", with: tracer)
    // Do not drain the worker while deliberately holding the export open.
    tracer.spanBuilder(spanName: "two").startSpan().end()
    XCTAssertEqual(started.wait(timeout: .now() + 1), .success)
    let completed = expectation(description: "tests continue while exporting")
    DispatchQueue.global().async {
      tracer.spanBuilder(spanName: "three").startSpan().end()
      tracer.spanBuilder(spanName: "dropped").startSpan().end()
      completed.fulfill()
    }
    wait(for: [completed], timeout: 1)
    release.signal()
    self.worker.sync {}
    // A partial remainder waits for its own timer, not the preceding request.
    XCTAssertEqual(exporter.exports.map(\.count), [2])
    release.signal()
    processor.forceFlush(timeout: 5)
    logger.waitForLogs()
    XCTAssertEqual(exporter.exports.flatMap { $0.map(\.name) }, ["one", "two", "three"])
    XCTAssertTrue(messages.value.contains { $0.contains("queue full") && $0.contains("dropped") })
  }

  func testBundleFinishFlushesRemainder() throws {
    let exporter = SequencedExporter(results: [])
    let client = try XCTUnwrap(TelemetryClient.live(
      environment: self.environment(["BUILDKITE_ANALYTICS_TOKEN": "secret", "BUILDKITE_ANALYTICS_KEY": "run"]),
      uploadTags: [:], logger: nil, rootExporter: exporter, childExporter: SequencedExporter(results: [])
    ))
    let observer = TestObserver(logger: nil, telemetry: client)
    observer.testCaseWillStart(self)
    observer.testCaseDidFinish(self)
    XCTAssertTrue(exporter.exports.isEmpty)
    observer.testBundleDidFinish(Bundle.main)
    XCTAssertEqual(exporter.exports.map(\.count), [1])
    XCTAssertEqual(exporter.exports[0][0].name, "test.execution")
  }

  func testOverridesTriggerSmallerBatchesAndBoundTheQueueDuringBackoff() {
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))])
    let processor = self.processor(exporter, batch: "2", queue: "3")
    let tracer = self.tracer(processor)
    for name in ["one", "two", "three", "dropped"] { self.endSpan(name, with: tracer) }
    XCTAssertEqual(exporter.exports.map(\.count), [2])
    processor.forceFlush(timeout: nil)
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one", "two"], ["one", "two"], ["three"]])
  }

  func testDefaultQueueHolds8192ExecutionsDuringBackoff() {
    let exporter = SequencedExporter(results: [.failure(.transportError)])
    let processor = self.processor(exporter)
    let tracer = self.tracer(processor)
    for index in 1...8193 { self.endSpan("test\(index)", with: tracer) }
    XCTAssertEqual(exporter.exports.map(\.count), [120])
    processor.forceFlush(timeout: 30)
    XCTAssertEqual(exporter.exports.dropFirst().flatMap { $0.map(\.name) }, (1...8192).map { "test\($0)" })
    XCTAssertEqual(exporter.exports.map(\.count).max(), 120)
  }

  func testSizeValidationMatchesRubyAndJavaScriptAndIgnoresOTELBSP() {
    let batchName = "BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE"
    let queueName = "BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE"
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    func sizes(_ values: [String: String]) -> [Int] {
      let sizes = ExecutionSpanProcessor.processorSizes(environment: self.environment(values), logger: logger)
      return [sizes.batch, sizes.queue]
    }
    XCTAssertEqual(sizes(["OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "1", "OTEL_BSP_MAX_QUEUE_SIZE": "1",
                          "OTEL_BSP_SCHEDULE_DELAY": "1"]), [120, 8192])
    XCTAssertEqual(sizes([batchName: "1", queueName: "1"]), [1, 1])
    XCTAssertEqual(sizes([batchName: "0002", queueName: "2147483647"]), [2, 2147483647])
    XCTAssertEqual(sizes([batchName: "2147483647", queueName: "2147483647"]), [2147483647, 2147483647])
    for invalid in ["", " ", " 2", "2 ", "0", "-1", "+1", "1.5", "1e2", "2147483648", "99999999999999999999", "１２"] {
      XCTAssertEqual(sizes([batchName: invalid]), [120, 8192], invalid)
      XCTAssertEqual(sizes([queueName: invalid]), [120, 8192], invalid)
    }
    XCTAssertEqual(sizes([batchName: "121", queueName: "120"]), [120, 8192])
    XCTAssertEqual(sizes([batchName: "bad", queueName: "240"]), [120, 240])
    logger.waitForLogs()
    XCTAssertEqual(messages.value.filter { $0.contains("must be a positive integer") }.count, 25)
    XCTAssertEqual(messages.value.filter { $0.contains("must be <=") }.count, 1)
  }

  func testBackoffProgressionAndResetAfterSuccess() {
    let clock = LockIsolated(TimeInterval(0))
    let exporter = SequencedExporter(results: [
      .failure(.httpStatus(503)), .failure(.httpStatus(503)), .failure(.httpStatus(503)),
      .failure(.httpStatus(503)), .failure(.httpStatus(503)), .success(()),
      .failure(.httpStatus(503)), .success(()),
    ])
    let processor = self.processor(exporter, batch: "1", now: { clock.value })
    let tracer = self.tracer(processor)
    for (attempt, time) in [0.0, 10, 30, 60, 120, 180].enumerated() {
      clock.setValue(time)
      self.endSpan("attempt\(attempt)", with: tracer)
      if attempt < 5 {
        XCTAssertEqual(exporter.exports.count, attempt + 1)
        clock.setValue([9, 29, 59, 119, 179][attempt])
        self.endSpan("beforeDeadline", with: tracer)
        XCTAssertEqual(exporter.exports.count, attempt + 1)
      }
    }
    // The successful first batch resets the next failure to a 10 s delay.
    self.worker.sync {}
    XCTAssertEqual(exporter.exports.count, 7)
    clock.setValue(189)
    self.endSpan("stillBackingOff", with: tracer)
    XCTAssertEqual(exporter.exports.count, 7)
    clock.setValue(190)
    self.endSpan("retry", with: tracer)
    processor.forceFlush(timeout: nil)
    XCTAssertEqual(exporter.exports[7][0].name, "beforeDeadline")
    XCTAssertEqual(exporter.exports.last?[0].name, "retry")
  }

  func testFlushBypassesExistingBackoffAndRetriesTransientFailuresWithinItsBudget() {
    for synchronous in [false, true] {
      let clock = LockIsolated(TimeInterval(0))
      let waits = LockIsolated([TimeInterval]())
      let exporter = SequencedExporter(results: [.failure(.transportError), .failure(.httpStatus(503))])
      let processor = self.processor(exporter, batch: "2", synchronous: synchronous, now: { clock.value }, sleep: { delay in
        waits.withValue { $0.append(delay) }
        clock.withValue { $0 += delay }
      })
      let tracer = self.tracer(processor)
      for name in ["one", "two", "three"] { self.endSpan(name, with: tracer) }
      processor.forceFlush(timeout: 30)
      XCTAssertEqual(waits.value, [synchronous ? 1 : 20])
      XCTAssertEqual(exporter.exports.dropFirst().map { $0.map(\.name) }, [["one", "two"], ["one", "two"], ["three"]])
      XCTAssertNil(exporter.lastFailure)
      processor.forceFlush(timeout: 30)
      XCTAssertEqual(exporter.exports.count, 4)
    }
  }

  func testFlushCapsRetryWaitsAtItsDeadlineAndReportsUnsentSpans() {
    for synchronous in [false, true] {
      let clock = LockIsolated(TimeInterval(0))
      let exporter = SequencedExporter(results: [.failure(.transportError)]) { _ in .failure(.httpStatus(503)) }
      let messages = LockIsolated([String]())
      let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
      let processor = self.processor(exporter, batch: "1", synchronous: synchronous, logger: logger, now: { clock.value }, sleep: { delay in
        clock.withValue { $0 += delay }
      })
      self.endSpan("unsent", with: self.tracer(processor))
      processor.forceFlush(timeout: 5)
      logger.waitForLogs()
      XCTAssertEqual(clock.value, 5)
      XCTAssertEqual(exporter.exports.count, synchronous ? 6 : 2)
      XCTAssertTrue(messages.value.contains { $0.contains("dropped 1") && $0.contains("unsent at flush") })
      processor.forceFlush(timeout: 5)
      XCTAssertEqual(exporter.exports.count, synchronous ? 6 : 2)
    }
  }

  func testByteCapAndTerminalRejectionsDropOnlyRejectedBatches() throws {
    let exporter = SequencedExporter(results: []) { spans in
      spans.contains { $0.name == "huge" } ? .failure(.httpStatus(413)) : .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    let processor = self.processor(exporter, logger: logger)
    let tracer = self.tracer(processor)
    for (name, size) in [("one", 200), ("two", 200), ("three", 200), ("huge", 700), ("last", 1)] {
      tracer.spanBuilder(spanName: name)
        .setAttribute(key: "payload", value: String(repeating: "x", count: size * 1024))
        .startSpan().end()
    }
    processor.forceFlush(timeout: 30)
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one", "two"], ["three"], ["huge"], ["last"]])
    for batch in exporter.exports where batch[0].name != "huge" {
      let request = Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest.with {
        $0.resourceSpans = SpanAdapter.toProtoResourceSpans(spanDataList: batch)
      }
      XCTAssertLessThanOrEqual(try request.serializedData().count, 512 * 1024)
    }
    logger.waitForLogs()
    XCTAssertTrue(messages.value.contains { $0.contains("export-failure, last OTLP failure: 413") })
  }

  func testTerminalFailuresBackOffAndReportDropTotalsPerRun() {
    let clock = LockIsolated(TimeInterval(0))
    let exporter = SequencedExporter(results: [
      .failure(.httpStatus(403)), .failure(.httpStatus(403)), .success(()), .failure(.httpStatus(401)),
    ])
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    let processor = self.processor(exporter, batch: "2", logger: logger, now: { clock.value })
    let tracer = self.tracer(processor)
    for name in ["one", "two", "three", "four"] { self.endSpan(name, with: tracer) }
    XCTAssertEqual(exporter.exports.count, 1)
    clock.setValue(10)
    self.endSpan("five", with: tracer)
    processor.forceFlush(timeout: nil)
    logger.waitForLogs()
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["one", "two"], ["three", "four"], ["five"]])
    XCTAssertTrue(messages.value.contains { $0.contains("dropped 2") && $0.contains("last OTLP failure: 403") })
    XCTAssertTrue(messages.value.contains { $0.contains("dropped 4 test.execution span(s) so far this run") })
    self.endSpan("nextRun", with: tracer)
    processor.forceFlush(timeout: nil)
    logger.waitForLogs()
    XCTAssertTrue(messages.value.contains { $0.contains("dropped 1") && $0.contains("last OTLP failure: 401") })
  }

  func testFlushDeadlineAndDropTotalsResetPerRun() {
    let clock = LockIsolated(TimeInterval(0))
    let exporter = SequencedExporter(results: [.failure(.httpStatus(503))]) { _ in
      clock.withValue { $0 += 8 }
      return .success(())
    }
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    let processor = self.processor(exporter, logger: logger, now: { clock.value })
    let tracer = self.tracer(processor)
    for index in 1...600 { self.endSpan("test\(index)", with: tracer) }
    processor.forceFlush(timeout: 30)
    logger.waitForLogs()
    XCTAssertEqual(exporter.exports.map(\.count), [120, 120, 120, 120, 120])
    XCTAssertEqual(exporter.timeouts, [nil, 30, 22, 14, 6])
    XCTAssertTrue(messages.value.contains { $0.contains("dropped 120") && $0.contains("unsent at flush") })
    let warningCount = messages.value.filter { $0.contains("warning:") }.count
    self.endSpan("nextRun", with: tracer)
    processor.forceFlush(timeout: 30)
    logger.waitForLogs()
    XCTAssertEqual(messages.value.filter { $0.contains("warning:") }.count, warningCount)
    XCTAssertEqual(exporter.exports.last?[0].name, "nextRun")
  }

  func testShutdownFlushesAndStopsAcceptingSpans() {
    let exporter = SequencedExporter(results: [])
    let processor = self.processor(exporter)
    let tracer = self.tracer(processor)
    self.endSpan("last", with: tracer)
    processor.shutdown(explicitTimeout: 5)
    self.endSpan("afterShutdown", with: tracer)
    processor.forceFlush(timeout: 5)
    XCTAssertEqual(exporter.exports.map { $0.map(\.name) }, [["last"]])
    XCTAssertTrue(exporter.didShutdown)
  }

  func testFlushAndShutdownDoNotStartAnotherRequestAfterWaitingExhaustsTheirBudget() {
    for shutdown in [false, true] {
      let clock = LockIsolated(TimeInterval(0))
      let flushing = LockIsolated(false)
      let started = DispatchSemaphore(value: 0)
      let release = DispatchSemaphore(value: 0)
      let exporter = SequencedExporter(results: [])
      let messages = LockIsolated([String]())
      let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
      let processor = self.processor(exporter, logger: logger, now: {
        let time = clock.value
        if flushing.value { release.signal() }
        return time
      })
      self.endSpan("unsent", with: self.tracer(processor))
      self.worker.async {
        started.signal()
        _ = release.wait(timeout: .now() + 5)
        clock.setValue(5)
      }
      XCTAssertEqual(started.wait(timeout: .now() + 1), .success)
      flushing.setValue(true)
      if shutdown {
        processor.shutdown(explicitTimeout: 5)
      } else {
        processor.forceFlush(timeout: 5)
      }
      logger.waitForLogs()
      XCTAssertTrue(exporter.exports.isEmpty)
      XCTAssertTrue(messages.value.contains { $0.contains("dropped 1") && $0.contains("unsent at flush") })
    }
  }

  private func environment(_ values: [String: String]) -> EnvironmentValues {
    EnvironmentValues(values: values, getFromEnvironment: { _ in nil }, getFromInfoDictionary: { _ in nil })
  }

  private func processor(
    _ exporter: SequencedExporter, batch: String? = nil, queue: String? = nil,
    synchronous: Bool = false,
    logger: Core.Logger? = nil,
    now: @escaping () -> TimeInterval = { ProcessInfo.processInfo.systemUptime },
    sleep: @escaping (TimeInterval) -> Void = { Thread.sleep(forTimeInterval: $0) }
  ) -> ExecutionSpanProcessor {
    var values = [String: String]()
    values["BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE"] = batch
    values["BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE"] = queue
    return ExecutionSpanProcessor(exporter: exporter, logger: logger, environment: self.environment(values), synchronous: synchronous, worker: self.worker, now: now, sleep: sleep)
  }

  private func tracer(_ processor: ExecutionSpanProcessor) -> any Tracer {
    TracerProviderSdk(spanProcessors: [processor]).get(instrumentationName: "ExecutionSpanProcessorTests")
  }

  private func endSpan(_ name: String, with tracer: any Tracer) {
    tracer.spanBuilder(spanName: name).setNoParent().startSpan().end()
    self.worker.sync {}
  }
}

/// Accessed on the export worker; tests inspect it only after joining that worker.
private final class SequencedExporter: FailureReportingSpanExporter, @unchecked Sendable {
  private var results: [Result<Void, SpanExportFailure>]
  private let handler: ([SpanData]) -> Result<Void, SpanExportFailure>
  private(set) var lastFailure: SpanExportFailure?
  var retryAfter: TimeInterval?
  var exports = [[SpanData]]()
  var timeouts = [TimeInterval?]()
  var didShutdown = false

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

  func flush(explicitTimeout: TimeInterval?) -> SpanExporterResultCode { .success }
  func shutdown(explicitTimeout: TimeInterval?) { self.didShutdown = true }
}
