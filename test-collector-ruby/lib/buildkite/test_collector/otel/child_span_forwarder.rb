# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      class ChildSpanForwarder
        def initialize(processor, context_key:, span_filter: nil)
          @processor = processor
          @context_key = context_key
          @span_filter = span_filter && SpanFilter.new(span_filter)
          @spans = {}
          @populated_phases = {}
          @mutex = Mutex.new
          @active = true
        end

        def on_start(span, parent_context)
          test_span_trace_id = parent_context.value(@context_key)
          return unless test_span_trace_id
          return unless test_span_trace_id == span.context.trace_id

          parent = OpenTelemetry::Trace.current_span(parent_context)
          @mutex.synchronize do
            next unless @active

            @spans[span] = true
            # A child that starts after its phase finished (async work from a
            # hook) must not re-add the phase, or it would live until shutdown.
            @populated_phases[parent] = true if phase_span?(parent) && @spans.key?(parent)
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not track OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        # Without a filter, a span is accepted and queued under one lock, so
        # shutdown cannot deactivate the forwarder in between and lose it.
        # A filter is caller code and runs outside the lock, so a slow filter
        # cannot stall other spans and one that finishes a span cannot
        # deadlock; a span still in its filter when shutdown runs is dropped.
        def on_finish(span)
          unless @span_filter
            @mutex.synchronize do
              @processor.on_finish(span) if @active && accept(span)
            end
            return
          end

          return unless @mutex.synchronize { @active && accept(span) }
          # Phase spans are structure the UI relies on; the filter never sees them.
          return unless phase_span?(span) || @span_filter.retain?(span)

          @mutex.synchronize do
            @processor.on_finish(span) if @active
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not export OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        def force_flush(timeout: nil)
          active = @mutex.synchronize { @active }
          return success unless active

          @processor.force_flush(timeout: timeout)
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not flush OpenTelemetry child spans: #{e.class}: #{e.message}"
          OpenTelemetry::SDK::Trace::Export::FAILURE
        end

        def shutdown(timeout: nil)
          @mutex.synchronize do
            @active = false
            @spans.clear
            @populated_phases.clear
          end
          success
        end

        private

        # Called under @mutex. A phase span that grouped nothing and did not
        # fail says nothing the test span does not, so an uninstrumented
        # suite exports no child spans at all.
        def accept(span)
          return false unless @spans.delete(span)
          return true unless phase_span?(span)

          @populated_phases.delete(span) || span.status.code != OpenTelemetry::Trace::Status::UNSET
        end

        def phase_span?(span)
          span.respond_to?(:name) && PHASE_SPAN_NAMES.value?(span.name)
        end

        def success
          OpenTelemetry::SDK::Trace::Export::SUCCESS
        end
      end
      private_constant :ChildSpanForwarder
    end
  end
end
