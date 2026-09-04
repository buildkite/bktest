# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Warns about dropped test spans and correlates processor drops with
      # exporter failures. Counts reset when reported at suite end or shutdown.
      class TestSpanMetricsReporter
        def initialize
          @mutex = Mutex.new
          @warned_count = nil
          @dropped_since_report = 0
          @last_export_failure = nil
        end

        def add_to_counter(metric, increment: 1, labels: {})
          case metric
          when "otel.otlp_exporter.failure"
            @mutex.synchronize { @last_export_failure = labels["reason"] }
          when "otel.bsp.export.success"
            # A retried request can fail and then succeed; only a failure
            # still standing when a batch is dropped explains that drop.
            @mutex.synchronize { @last_export_failure = nil }
          when "otel.bsp.dropped_spans"
            record_drop(increment, labels["reason"])
          end
        end

        # A test span that never started is as missing from Buildkite as one
        # the processor dropped, so it is counted and reported the same way.
        def record_start_failure(error)
          record_drop(1, "could not start span: #{error.class}: #{error.message}")
        end

        def record_value(_metric, value:, labels: {}); end

        def observe_value(_metric, value:, labels: {}); end

        def warn_dropped_total
          total, warned = @mutex.synchronize do
            counts = [@dropped_since_report, @warned_count]
            @dropped_since_report = 0
            @warned_count = nil
            counts
          end
          return if total.zero? || total == warned

          warn "[buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{total} test.execution span(s) " \
            "so far this run; those test executions were not uploaded to Buildkite Test Engine."
        end

        private

        def record_drop(count, reason)
          export_failure = @mutex.synchronize do
            @dropped_since_report += count
            return if @warned_count

            @warned_count = count
            @last_export_failure
          end

          # The exporter's reason is an HTTP status for rejected requests and an
          # exception class name for connection errors, so "failure" covers both.
          cause = export_failure if reason == "export-failure"
          detail = [reason, cause && "last OTLP failure: #{cause}"].compact.join(", ")

          warn <<~MESSAGE.chomp
            [buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{count} test.execution span(s) (#{detail}).
            [buildkite-test_collector] OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine.
          MESSAGE
        end
      end
      private_constant :TestSpanMetricsReporter
    end
  end
end
