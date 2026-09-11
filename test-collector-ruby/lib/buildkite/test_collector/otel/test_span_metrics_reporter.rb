# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # A dropped test span is a test execution missing from Buildkite, so the
      # warnings say so.
      class TestSpanMetricsReporter < SpanMetricsReporter
        # A test span that never started is as missing from Buildkite as one
        # the processor dropped, so it is counted and reported the same way.
        def record_start_failure(error)
          record_drop(1, "could not start span: #{error.class}: #{error.message}")
        end

        private

        def dropped_message(count, detail)
          <<~MESSAGE.chomp
            [buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{count} test.execution span(s) (#{detail}).
            [buildkite-test_collector] OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.
          MESSAGE
        end

        def dropped_total_message(total)
          "[buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{total} test.execution span(s) " \
            "so far this run; those test executions were not uploaded to Buildkite Test Engine."
        end
      end
      private_constant :TestSpanMetricsReporter
    end
  end
end
