import Foundation
import OpenTelemetryApi
import OpenTelemetryProtocolExporterCommon
import OpenTelemetrySdk

/// Why the most recent export attempt failed. Exporters that cannot tell
/// report nothing and are retried.
enum SpanExportFailure: Error {
  case timeout
  case transportError
  case httpStatus(Int)

  // Only transient failures are retried; any other status (400 bad run key,
  // 401/403 auth, 413, 415, 500, …) would recur.
  var isRetryable: Bool {
    switch self {
    case .timeout, .transportError:
      return true
    case let .httpStatus(status):
      return [408, 429, 502, 503, 504].contains(status)
    }
  }

  var reason: String {
    switch self {
    case .timeout:
      return "timeout"
    case .transportError:
      return "socket_error"
    case let .httpStatus(status):
      return String(status)
    }
  }
}

protocol FailureReportingSpanExporter: SpanExporter {
  var lastFailure: SpanExportFailure? { get }
}

/// Exports finished `test.execution` spans off the test thread, at the batch
/// threshold or one second after the first queued span. Spans a
/// transient failure leaves behind are retried after a backoff; a terminal
/// failure drops only the batch it rejected.
final class ExecutionSpanProcessor: SpanProcessor {
  /// A batch never exceeds this many spans, nor this many encoded bytes
  /// before compression, so a backlog accumulated during an outage cannot
  /// become one request over the receiver's 900 KiB limit. A single span
  /// larger than the byte cap is sent on its own.
  static let maxBatchSpanCount = 120
  static let maxQueueSize = 8192
  static let maxBatchEncodedSize = 512 * 1024

  private static let retryDelays: [TimeInterval] = [10, 20, 30, 60]

  let isStartRequired = false
  let isEndRequired = true

  private struct PendingSpan {
    let data: SpanData
    let encodedSize: Int
    let queuedAt: TimeInterval
  }

  private let exporter: any SpanExporter
  private let failureReporter: (any FailureReportingSpanExporter)?
  private let exporterLock = NSLock()
  private let worker: DispatchQueue
  private let timer: DispatchSourceTimer
  private let batchSize: Int
  private let queueSize: Int
  private var exportScheduled = false
  private var stopped = false
  private let logger: Logger?
  private let now: () -> TimeInterval
  private var pending = [PendingSpan]()
  private var retryDelayIndex = 0
  private var retryNotBefore: TimeInterval?
  private var droppedCount = 0
  private var reportedDroppedCount = 0

  init(
    exporter: any SpanExporter,
    logger: Logger?,
    environment: EnvironmentValues = EnvironmentValues(),
    worker: DispatchQueue = DispatchQueue(label: "BuildkiteTestCollector.execution-export"),
    now: @escaping () -> TimeInterval = { ProcessInfo.processInfo.systemUptime }
  ) {
    self.exporter = exporter
    self.failureReporter = exporter as? any FailureReportingSpanExporter
    self.logger = logger
    let sizes = Self.processorSizes(environment: environment, logger: logger)
    self.batchSize = sizes.batch
    self.queueSize = sizes.queue
    self.worker = worker
    self.timer = DispatchSource.makeTimerSource(queue: worker)
    self.now = now
    self.timer.setEventHandler { [weak self] in self?.exportQueued(minimumBatchSize: 1) }
    self.timer.resume()
  }

  deinit {
    self.timer.cancel()
  }

  static func processorSizes(environment: EnvironmentValues, logger: Logger?) -> (batch: Int, queue: Int) {
    let batchName = "BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE"
    let queueName = "BUILDKITE_TESTS_OTEL_TEST_SPAN_QUEUE_SIZE"
    func size(_ name: String, default fallback: Int) -> Int {
      guard let raw = environment.string(for: name, trimWhitespace: false) else { return fallback }
      if !raw.isEmpty, raw.utf8.allSatisfy({ $0 >= 48 && $0 <= 57 }),
        let value = Int(raw), (1...2_147_483_647).contains(value) {
        return value
      }
      logger?.warning("\(name) must be a positive integer up to 2147483647, using default \(fallback)")
      return fallback
    }
    let batch = size(batchName, default: Self.maxBatchSpanCount)
    let queue = size(queueName, default: Self.maxQueueSize)
    guard batch <= queue else {
      logger?.warning("\(batchName) must be <= \(queueName); using defaults (batch 120, queue 8192)")
      return (Self.maxBatchSpanCount, Self.maxQueueSize)
    }
    return (batch, queue)
  }

  func onStart(parentContext: SpanContext?, span: any ReadableSpan) {}

  func onEnd(span: any ReadableSpan) {
    self.exporterLock.lock()
    defer { self.exporterLock.unlock() }

    guard !self.stopped else { return }
    let data = span.toSpanData()
    guard self.pending.count < self.queueSize else {
      self.droppedCount += 1
      self.reportDropped(count: 1, detail: "queue full", batch: [PendingSpan(data: data, encodedSize: 0, queuedAt: self.now())][...])
      return
    }
    if self.pending.isEmpty {
      self.timer.schedule(deadline: .now() + 1)
    }
    self.pending.append(PendingSpan(data: data, encodedSize: Self.encodedSize(of: data), queuedAt: self.now()))
    let name = Self.testName(data)
    if let retryNotBefore = self.retryNotBefore, self.now() < retryNotBefore {
      self.logger?.debug(
        "Queued OpenTelemetry test execution during export backoff: \(name) (\(self.pending.count) pending)"
      )
      return
    }

    self.scheduleFullBatch()
  }

  // Called with exporterLock held. Coalesce threshold notifications and yield
  // between requests so a final flush can take over an active backlog.
  private func scheduleFullBatch() {
    if self.pending.count >= self.batchSize, !self.exportScheduled {
      self.exportScheduled = true
      self.worker.async { [weak self] in
        guard let self else { return }
        self.exportQueued(minimumBatchSize: self.batchSize)
      }
    }
  }

  private func exportQueued(minimumBatchSize: Int) {
    self.exporterLock.lock()
    self.exportScheduled = true
    defer {
      self.exportScheduled = false
      if self.retryNotBefore == nil { self.scheduleFullBatch() }
      self.exporterLock.unlock()
    }
    self.timer.schedule(deadline: .distantFuture)
    guard !self.stopped, !self.pending.isEmpty else { return }
    if let retryNotBefore = self.retryNotBefore, self.now() < retryNotBefore {
      self.timer.schedule(deadline: .now() + (retryNotBefore - self.now()))
      return
    }
    switch self.exportPending(minimumBatchSize: minimumBatchSize, drain: false) {
    case .exported:
      self.logger?.debug("Exported queued OpenTelemetry test executions")
    case .retryLater:
      let failure = self.failureReporter?.lastFailure
      self.logger?.error(
        "OpenTelemetry export failed\(failure.map { " (\($0.reason))" } ?? ""); \(self.pending.count) execution(s) remain queued in this process"
      )
    case .dropped:
      break
    }
    if !self.pending.isEmpty, let retryNotBefore = self.retryNotBefore {
      self.timer.schedule(deadline: .now() + max(retryNotBefore - self.now(), 0))
    } else if let first = self.pending.first {
      self.timer.schedule(deadline: .now() + max(first.queuedAt + 1 - self.now(), 0))
    } else if self.pending.isEmpty {
      self.timer.schedule(deadline: .distantFuture)
    }
  }

  func forceFlush(timeout: TimeInterval?) {
    let deadline = timeout.map { self.now() + $0 }
    self.worker.sync {
      self.exporterLock.lock()
      defer { self.exporterLock.unlock() }
      self.timer.schedule(deadline: .distantFuture)
      self.flush(timeout: deadline.map { max($0 - self.now(), 0) })
    }
  }

  func shutdown(explicitTimeout: TimeInterval?) {
    self.worker.sync {
      self.exporterLock.lock()
      defer { self.exporterLock.unlock() }
      self.stopped = true
      self.timer.cancel()
      self.flush(timeout: explicitTimeout)
      self.exporter.shutdown(explicitTimeout: explicitTimeout)
    }
  }

  // The flush is the last chance before the process exits, and under
  // `swift test --parallel` every worker process pays for it, so it makes one
  // immediate pass over the queue and never waits out a backoff. A terminal
  // failure rejected only its own batch, so the pass goes on; a transient
  // failure or the deadline ends it. Anything still unsent is lost; count it
  // as dropped.
  private func flush(timeout: TimeInterval?) {
    let deadline = timeout.map { self.now() + $0 }
    var attempted = false
    while !self.pending.isEmpty {
      if attempted, let deadline, self.now() >= deadline { break }
      attempted = true
      guard self.exportPending(deadline: deadline) == .dropped else { break }
    }
    if !self.pending.isEmpty {
      let batch = self.pending[...]
      self.reportDropped(
        count: self.drop(batch),
        detail: "unsent at flush\(Self.detail(self.failureReporter?.lastFailure))",
        batch: batch
      )
    }
    if self.droppedCount > self.reportedDroppedCount {
      self.logger?.warning(
        "TEST RESULTS MISSING: OpenTelemetry dropped \(self.droppedCount) test.execution span(s) so far this run; those test executions were not uploaded to Buildkite Test Engine."
      )
    }
    // Each flush closes a run; the next one starts a fresh count.
    self.droppedCount = 0
    self.reportedDroppedCount = 0
    if case .failure = self.exporter.flush(explicitTimeout: timeout) {
      self.logger?.error("OpenTelemetry export failed while flushing pending test executions")
    }
  }

  private enum ExportOutcome {
    case exported
    case retryLater
    case dropped
  }

  /// Exports batches from the head of the queue until it is empty, the
  /// deadline passes, or a batch fails. A failed batch starts a backoff
  /// either way so a rejecting receiver is not hit once per test; a terminal
  /// failure additionally drops that batch, and the rest stay queued for the
  /// next attempt.
  @discardableResult
  private func exportPending(deadline: TimeInterval? = nil, minimumBatchSize: Int = 1, drain: Bool = true) -> ExportOutcome {
    while self.pending.count >= minimumBatchSize {
      let batch = self.nextBatch()
      let timeout = deadline.map { max($0 - self.now(), 0) }
      // Only the serial worker calls the exporter. Keep the bounded queue
      // available to test threads while the network request is in flight.
      self.exporterLock.unlock()
      let result = self.exporter.export(spans: batch.map(\.data), explicitTimeout: timeout)
      self.exporterLock.lock()
      if case .success = result {
        self.pending.removeFirst(batch.count)
        self.retryDelayIndex = 0
        self.retryNotBefore = nil
        if !drain { return .exported }
        if let deadline, self.now() >= deadline { return .retryLater }
        continue
      }

      let delay = Self.retryDelays[self.retryDelayIndex]
      self.retryNotBefore = self.now() + delay
      self.retryDelayIndex = min(self.retryDelayIndex + 1, Self.retryDelays.count - 1)
      let failure = self.failureReporter?.lastFailure
      guard let failure, !failure.isRetryable else { return .retryLater }
      self.reportDropped(
        count: self.drop(batch),
        detail: "export-failure\(Self.detail(failure))",
        batch: batch
      )
      return .dropped
    }
    return .exported
  }

  private func nextBatch() -> ArraySlice<PendingSpan> {
    var end = self.pending.startIndex
    var size = 0
    while end < self.pending.endIndex, end - self.pending.startIndex < self.batchSize {
      size += self.pending[end].encodedSize
      if size > Self.maxBatchEncodedSize, end > self.pending.startIndex { break }
      end += 1
    }
    return self.pending[self.pending.startIndex..<end]
  }

  private func drop(_ batch: ArraySlice<PendingSpan>) -> Int {
    self.pending.removeSubrange(batch.indices)
    self.droppedCount += batch.count
    return batch.count
  }

  // The first drop is reported immediately so the cause is visible next to
  // the test that triggered it; the run total is reported at flush.
  private func reportDropped(count: Int, detail: String, batch: ArraySlice<PendingSpan>) {
    let range = batch.count == 1
      ? Self.testName(batch[batch.startIndex].data)
      : "\(Self.testName(batch[batch.startIndex].data)) … \(Self.testName(batch[batch.endIndex - 1].data))"
    guard self.reportedDroppedCount == 0 else {
      self.logger?.debug("Dropped \(count) OpenTelemetry test execution(s) (\(detail)): \(range)")
      return
    }
    self.reportedDroppedCount = self.droppedCount
    self.logger?.warning(
      "TEST RESULTS MISSING: OpenTelemetry dropped \(count) test.execution span(s) (\(detail)): \(range). OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine."
    )
  }

  private static func detail(_ failure: SpanExportFailure?) -> String {
    failure.map { ", last OTLP failure: \($0.reason)" } ?? ""
  }

  private static func testName(_ span: SpanData) -> String {
    span.attributes[BuildkiteTelemetryAttribute.testName]?.description ?? span.name
  }

  private static func encodedSize(of span: SpanData) -> Int {
    let request = Opentelemetry_Proto_Collector_Trace_V1_ExportTraceServiceRequest.with {
      $0.resourceSpans = SpanAdapter.toProtoResourceSpans(spanDataList: [span])
    }
    return (try? request.serializedData().count) ?? 0
  }
}
