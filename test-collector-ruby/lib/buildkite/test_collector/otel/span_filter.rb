# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Decides which finished child spans are forwarded for export by asking
      # the suite's otel_span_filter. Filtering only trims export volume, so
      # it must never cost the suite a span: a filter that fails retains the
      # span and warns once, and a span finished while the filter is running
      # (an instrumented call inside the filter) is retained rather than fed
      # back into the filter, which would recurse until SystemStackError.
      class SpanFilter
        # Takes the filter's place when none is configured.
        module RetainAll
          def self.retain?(_span) = true
        end

        RUNNING = :buildkite_test_collector_span_filter_running
        private_constant :RUNNING

        def self.from(callable) = callable ? new(callable) : RetainAll

        def initialize(callable)
          @callable = callable
          @reported = false
        end

        def retain?(span)
          return true if Thread.current[RUNNING]

          running { @callable.call(span) }
        rescue StandardError => e
          report_once(e)
          true
        end

        private

        def running
          Thread.current[RUNNING] = true
          yield
        ensure
          Thread.current[RUNNING] = nil
        end

        # A broken filter fails the same way for every span. Two threads may
        # both report the first failure; that is harmless.
        def report_once(error)
          return if @reported

          @reported = true
          warn "[buildkite-test_collector] Could not filter OpenTelemetry child span, retaining it: #{error.class}: #{error.message}. Further filter failures will not be reported."
        end
      end
      private_constant :SpanFilter
    end
  end
end
