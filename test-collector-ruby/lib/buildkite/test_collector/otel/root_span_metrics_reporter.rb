# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # BatchSpanProcessor silently drops spans by default. Roots are the
      # submission itself and have no JSON fallback, so a dropped root is a
      # missing test execution: warn once per suite run, prominently, and say
      # why, then report that run's full count at its end so a persistent
      # failure that drops every batch is not mistaken for a one-off.
      #
      # Shared by the root exporter and its processor: the exporter reports
      # the HTTP status of a rejected request, then the processor reports the
      # batch it dropped as a result.
      class RootSpanMetricsReporter
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
            warn_dropped(increment, labels["reason"])
          end
        end

        def record_value(_metric, value:, labels: {}); end

        def observe_value(_metric, value:, labels: {}); end

        # Called after each suite's flush and after shutdown, so the count
        # covers everything that suite run dropped. Silent when the inline
        # warning already named every one. Re-arms the inline warning, so a
        # warm worker's next suite run reports its own losses too.
        def warn_total
          total, warned = @mutex.synchronize do
            counts = [@dropped_since_report, @warned_count]
            @dropped_since_report = 0
            @warned_count = nil
            counts
          end
          return if total.zero? || total == warned

          warn "[buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{total} test.execution span(s) " \
            "in total; those test executions were not uploaded to Buildkite Test Engine."
        end

        private

        def warn_dropped(count, reason)
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
      private_constant :RootSpanMetricsReporter
    end
  end
end
