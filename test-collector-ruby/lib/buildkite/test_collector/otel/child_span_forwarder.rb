# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      class ChildSpanForwarder
        # Marks the thread while its filter runs. An instrumented call inside
        # the filter starts and finishes a span on this same thread, so without
        # the mark that span's on_finish would run the filter again, recursing
        # until SystemStackError. Such spans are retained unfiltered instead.
        FILTER_RUNNING = :buildkite_test_collector_span_filter_running
        private_constant :FILTER_RUNNING

        def initialize(processor, context_key:, span_filter: nil)
          @processor = processor
          @context_key = context_key
          @span_filter = span_filter
          @filter_failed = false
          @spans = {}
          @mutex = Mutex.new
          @active = true
        end

        def on_start(span, parent_context)
          test_span_trace_id = parent_context.value(@context_key)
          return unless test_span_trace_id
          return unless test_span_trace_id == span.context.trace_id

          @mutex.synchronize do
            @spans[span] = true if @active
          end
        rescue StandardError => e
          warn "[buildkite-test_collector] Could not track OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        def on_finish(span)
          return unless @mutex.synchronize { @active && @spans.delete(span) }
          # The filter is caller code: run it outside the lock so a slow filter
          # cannot stall other spans, and one that starts a span cannot re-enter.
          return unless retain?(span)

          @mutex.synchronize do
            @processor.on_finish(span) if @active
          end
        rescue StandardError => e
          warn "[buildkite-test_collector] Could not export OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        def force_flush(timeout: nil)
          active = @mutex.synchronize { @active }
          return success unless active

          @processor.force_flush(timeout: timeout)
        rescue StandardError => e
          warn "[buildkite-test_collector] Could not flush OpenTelemetry child spans: #{e.class}: #{e.message}"
          OpenTelemetry::SDK::Trace::Export::FAILURE
        end

        def shutdown(timeout: nil)
          @mutex.synchronize do
            @active = false
            @spans.clear
          end
          success
        end

        private

        # A broken filter fails the same way for every span, so report it once
        # rather than once per span. The racy flag is fine: at worst two threads
        # both warn.
        def retain?(span)
          return true if !@span_filter || Thread.current[FILTER_RUNNING]

          Thread.current[FILTER_RUNNING] = true
          begin
            @span_filter.call(span)
          ensure
            Thread.current[FILTER_RUNNING] = nil
          end
        rescue StandardError => e
          unless @filter_failed
            @filter_failed = true
            warn "[buildkite-test_collector] Could not filter OpenTelemetry child span, retaining it: #{e.class}: #{e.message}. Further filter failures will not be reported."
          end
          true
        end

        def success
          OpenTelemetry::SDK::Trace::Export::SUCCESS
        end
      end
      private_constant :ChildSpanForwarder
    end
  end
end
