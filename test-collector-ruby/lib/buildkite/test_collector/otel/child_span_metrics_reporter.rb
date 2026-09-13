# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Child spans are best-effort detail attached to test executions, so a
      # drop is worth a warning but does not lose a test result.
      class ChildSpanMetricsReporter < SpanMetricsReporter
        private

        def dropped_message(count, detail)
          "[buildkite-test_collector] OpenTelemetry dropped #{count} child span(s) (#{detail}); " \
            "test.execution results are unaffected."
        end

        def dropped_total_message(total)
          "[buildkite-test_collector] OpenTelemetry dropped #{total} child span(s) so far this run; " \
            "test.execution results are unaffected."
        end
      end
      private_constant :ChildSpanMetricsReporter
    end
  end
end
