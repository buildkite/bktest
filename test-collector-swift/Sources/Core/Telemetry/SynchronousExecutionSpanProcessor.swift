import Foundation
import OpenTelemetryApi
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

final class SynchronousExecutionSpanProcessor: SpanProcessor {
  private static let retryDelays: [TimeInterval] = [10, 20, 30, 60]

  let isStartRequired = false
  let isEndRequired = true

  private let exporter: any SpanExporter
  private let failureReporter: (any FailureReportingSpanExporter)?
  private let exporterLock = NSLock()
  private let logger: Logger?
  private let now: () -> TimeInterval
  private var pending = [SpanData]()
  private var retryDelayIndex = 0
  private var retryNotBefore: TimeInterval?
  private var droppedCount = 0
  private var reportedDroppedCount = 0

  init(
    exporter: any SpanExporter,
    logger: Logger?,
    now: @escaping () -> TimeInterval = { ProcessInfo.processInfo.systemUptime }
  ) {
    self.exporter = exporter
    self.failureReporter = exporter as? any FailureReportingSpanExporter
    self.logger = logger
    self.now = now
  }

  func onStart(parentContext: SpanContext?, span: any ReadableSpan) {}

  func onEnd(span: any ReadableSpan) {
    self.exporterLock.lock()
    defer { self.exporterLock.unlock() }

    let name = span.getAttributes()[BuildkiteTelemetryAttribute.testName]?.description ?? span.name
    self.pending.append(span.toSpanData())
    if let retryNotBefore = self.retryNotBefore, self.now() < retryNotBefore {
      self.logger?.debug(
        "Queued OpenTelemetry test execution during export backoff: \(name) (\(self.pending.count) pending)"
      )
      return
    }

    if self.exportPending() {
      self.logger?.debug("Exported OpenTelemetry test execution: \(name)")
      return
    }

    let failure = self.failureReporter?.lastFailure
    if failure?.isRetryable ?? true {
      self.logger?.error(
        "OpenTelemetry export failed for \(name)\(failure.map { " (\($0.reason))" } ?? ""); the execution remains queued in this process"
      )
    } else {
      self.reportDropped(count: self.dropPending(), failure: failure)
    }
  }

  func forceFlush(timeout: TimeInterval?) {
    self.exporterLock.lock()
    defer { self.exporterLock.unlock() }
    self.flush(timeout: timeout)
  }

  func shutdown(explicitTimeout: TimeInterval?) {
    self.exporterLock.lock()
    defer { self.exporterLock.unlock() }
    self.flush(timeout: explicitTimeout)
    self.exporter.shutdown(explicitTimeout: explicitTimeout)
  }

  // The process exits after the bundle flush, so anything still unsent here
  // is lost; count it as dropped.
  private func flush(timeout: TimeInterval?) {
    if !self.pending.isEmpty, !self.exportPending(timeout: timeout) {
      self.reportDropped(count: self.dropPending(), failure: self.failureReporter?.lastFailure)
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

  private func exportPending(timeout: TimeInterval? = nil) -> Bool {
    switch self.exporter.export(spans: self.pending, explicitTimeout: timeout) {
    case .success:
      self.pending.removeAll()
      self.retryDelayIndex = 0
      self.retryNotBefore = nil
      return true
    case .failure:
      let delay = Self.retryDelays[self.retryDelayIndex]
      self.retryNotBefore = self.now() + delay
      self.retryDelayIndex = min(self.retryDelayIndex + 1, Self.retryDelays.count - 1)
      return false
    }
  }

  private func dropPending() -> Int {
    let count = self.pending.count
    self.pending.removeAll()
    self.droppedCount += count
    return count
  }

  // The first drop is reported immediately so the cause is visible next to
  // the test that triggered it; the run total is reported at flush.
  private func reportDropped(count: Int, failure: SpanExportFailure?) {
    guard self.reportedDroppedCount == 0 else {
      self.logger?.debug("Dropped \(count) OpenTelemetry test execution(s)\(Self.detail(failure))")
      return
    }
    self.reportedDroppedCount = self.droppedCount
    self.logger?.warning(
      "TEST RESULTS MISSING: OpenTelemetry dropped \(count) test.execution span(s) (export-failure\(Self.detail(failure))). OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine."
    )
  }

  private static func detail(_ failure: SpanExportFailure?) -> String {
    failure.map { ", last OTLP failure: \($0.reason)" } ?? ""
  }
}
