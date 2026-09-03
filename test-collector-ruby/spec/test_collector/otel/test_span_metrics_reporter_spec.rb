# frozen_string_literal: true

reporter_class = Buildkite::TestCollector::OTel.const_get(:TestSpanMetricsReporter, false)

RSpec.describe reporter_class do
  subject(:reporter) { described_class.new }

  describe "#add_to_counter" do
    it "warns only once when test spans are dropped" do
      expect do
        2.times do
          reporter.add_to_counter(
            "otel.bsp.dropped_spans",
            increment: 3,
            labels: { "reason" => "buffer-full" },
          )
        end
      end.to output(
        "[buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped 3 test.execution span(s) (buffer-full).\n" \
          "[buildkite-test_collector] OpenTelemetry is the only upload path, so those test executions were not uploaded " \
          "to Buildkite Test Engine.\n"
      ).to_stderr
    end

    it "names the HTTP status or error that caused an export failure" do
      # The OTLP exporter reports the rejected request, then the processor
      # reports the batch it dropped as a result.
      reporter.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => "403" })

      expect do
        reporter.add_to_counter(
          "otel.bsp.dropped_spans",
          increment: 12,
          labels: { "reason" => "export-failure" },
        )
      end.to output(
        /dropped 12 test\.execution span\(s\) \(export-failure, last OTLP failure: 403\)\./
      ).to_stderr
    end

    it "forgets a failure once a retried export succeeds" do
      # A 503 that the exporter retried successfully must not be blamed for a
      # later drop whose own failure path records no metric.
      reporter.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => "503" })
      reporter.add_to_counter("otel.bsp.export.success")

      expect do
        reporter.add_to_counter(
          "otel.bsp.dropped_spans",
          increment: 1,
          labels: { "reason" => "export-failure" },
        )
      end.to output(/\(export-failure\)\./).to_stderr
    end

    it "does not blame an earlier export failure for a full buffer" do
      reporter.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => "503" })

      expect do
        reporter.add_to_counter(
          "otel.bsp.dropped_spans",
          increment: 1,
          labels: { "reason" => "buffer-full" },
        )
      end.to output(/\(buffer-full\)\./).to_stderr
    end

    it "ignores other processor metrics" do
      expect do
        reporter.add_to_counter("otel.bsp.export.success")
        reporter.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => "403" })
      end.not_to output.to_stderr
    end
  end

  describe "#warn_dropped_total" do
    def drop(count, reason: "export-failure")
      reporter.add_to_counter("otel.bsp.dropped_spans", increment: count, labels: { "reason" => reason })
    end

    it "reports every test span the suite run dropped" do
      # A persistent failure drops every batch, but only the first is warned
      # about inline; the total is what tells the reader the whole run is gone.
      expect { drop(512) }.to output(/dropped 512 test\.execution span\(s\)/).to_stderr
      drop(512)
      drop(7, reason: "terminating")

      expect { reporter.warn_dropped_total }.to output(
        "[buildkite-test_collector] TEST RESULTS MISSING: OpenTelemetry dropped 1031 test.execution span(s) " \
          "so far this run; those test executions were not uploaded to Buildkite Test Engine.\n"
      ).to_stderr
    end

    it "stays silent when the first warning already named every dropped test span" do
      expect { drop(3) }.to output.to_stderr

      expect { reporter.warn_dropped_total }.not_to output.to_stderr
    end

    it "starts a fresh count and inline warning for the next suite run" do
      # Warm workers run several suites per process; each run's log must
      # state its own losses rather than depending on process exit.
      expect { drop(512) }.to output.to_stderr
      drop(512)
      expect { reporter.warn_dropped_total }.to output(/dropped 1024 test\.execution span\(s\) so far this run/).to_stderr

      expect { drop(2) }.to output(/dropped 2 test\.execution span\(s\) \(export-failure\)/).to_stderr
      expect { reporter.warn_dropped_total }.not_to output.to_stderr
    end

    it "stays silent when nothing was dropped" do
      reporter.add_to_counter("otel.bsp.export.success")

      expect { reporter.warn_dropped_total }.not_to output.to_stderr
    end
  end
end
