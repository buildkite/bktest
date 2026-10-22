import Foundation
import OpenTelemetryApi
import OpenTelemetrySdk

final class ExecutionTraceRegistry {
  private let traceIDs = LockIsolated(Set<TraceId>())

  func insert(_ traceID: TraceId) {
    _ = self.traceIDs.withValue { $0.insert(traceID) }
  }

  func remove(_ traceID: TraceId) {
    _ = self.traceIDs.withValue { $0.remove(traceID) }
  }

  func contains(_ traceID: TraceId) -> Bool {
    self.traceIDs.withValue { $0.contains(traceID) }
  }
}

final class ExecutionChildSpanProcessor: SpanProcessor {
  let isStartRequired = true
  let isEndRequired = true

  private let registry: ExecutionTraceRegistry
  private let state: LockIsolated<State>

  private struct State {
    var active = true
    var acceptedSpanIDs = Set<SpanId>()
    var forwardedSpanCount = 0
    var processor: any SpanProcessor
  }

  init(processor: any SpanProcessor, registry: ExecutionTraceRegistry) {
    self.registry = registry
    self.state = LockIsolated(State(processor: processor))
  }

  /// How many finished child spans have been handed to the processor.
  var forwardedSpanCount: Int {
    self.state.forwardedSpanCount
  }

  func onStart(parentContext: SpanContext?, span: any ReadableSpan) {
    guard
      let parentContext,
      parentContext.traceId == span.context.traceId,
      self.registry.contains(span.context.traceId)
    else { return }

    self.state.withValue { state in
      if state.active {
        state.acceptedSpanIDs.insert(span.context.spanId)
      }
    }
  }

  func onEnd(span: any ReadableSpan) {
    self.state.withValue { state in
      guard state.active, state.acceptedSpanIDs.remove(span.context.spanId) != nil else { return }
      state.forwardedSpanCount += 1
      state.processor.onEnd(span: span)
    }
  }

  // The flush runs outside the lock so an abandoned slow flush does not
  // block spans that finish after it.
  func forceFlush(timeout: TimeInterval?) {
    guard let processor = self.state.withValue({ $0.active ? $0.processor : nil }) else { return }
    processor.forceFlush(timeout: timeout)
  }

  func shutdown(explicitTimeout: TimeInterval?) {
    self.state.withValue { state in
      state.active = false
      state.acceptedSpanIDs.removeAll()
      state.processor.shutdown(explicitTimeout: explicitTimeout)
    }
  }
}
