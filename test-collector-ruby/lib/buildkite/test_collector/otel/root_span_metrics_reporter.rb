# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # BatchSpanProcessor silently drops spans by default. Roots are the
      # submission itself and have no JSON fallback, so a dropped root is a
      # missing test execution: warn once, prominently, and say why.
      #
      # Shared by the root exporter and its processor: the exporter reports
      # the HTTP status of a rejected request, then the processor reports the
      # batch it dropped as a result.
      class RootSpanMetricsReporter
        def initialize
          @mutex = Mutex.new
          @warned = false
          @last_export_failure = nil
        end

        def add_to_counter(metric, increment: 1, labels: {})
          case metric
          when "otel.otlp_exporter.failure"
            @mutex.synchronize { @last_export_failure = labels["reason"] }
          when "otel.bsp.dropped_spans"
            warn_dropped(increment, labels["reason"])
          end
        end

        def record_value(_metric, value:, labels: {}); end

        def observe_value(_metric, value:, labels: {}); end

        private

        def warn_dropped(count, reason)
          should_warn, export_failure = @mutex.synchronize do
            next [false, nil] if @warned

            @warned = true
            [true, @last_export_failure]
          end
          return unless should_warn

          detail = reason.to_s
          # The exporter's reason is an HTTP status for rejected requests and an
          # exception class name for connection errors, so "failure" covers both.
          detail += ", last OTLP failure: #{export_failure}" if reason == "export-failure" && export_failure

          warn <<~MESSAGE.chomp
            [buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped #{count} test.execution span(s) (#{detail}).
            [buildkite-test_collector] OpenTelemetry is the only upload path, so those test executions were not uploaded to Buildkite Test Engine. See the OpenTelemetry errors above for details.
          MESSAGE
        end
      end
      private_constant :RootSpanMetricsReporter
    end
  end
end
