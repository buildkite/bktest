# frozen_string_literal: true

reporter_class = Buildkite::TestCollector::OTel.const_get(:ChildSpanMetricsReporter, false)

RSpec.describe reporter_class do
  subject(:reporter) { described_class.new }

  def drop(count, reason: "buffer-full")
    reporter.add_to_counter("otel.bsp.dropped_spans", increment: count, labels: { "reason" => reason })
  end

  it "warns once, as best-effort, when child spans are dropped" do
    expect { 2.times { drop(3) } }.to output(
      "[buildkite-test_collector] OpenTelemetry dropped 3 child span(s) (buffer-full); " \
        "test.execution results are unaffected.\n"
    ).to_stderr
  end

  it "names the export failure behind a dropped batch" do
    reporter.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => "413" })

    expect { drop(512, reason: "export-failure") }.to output(
      /dropped 512 child span\(s\) \(export-failure, last OTLP failure: 413\); test\.execution results are unaffected\./
    ).to_stderr
  end

  it "reports the run total when it exceeds the inline warning" do
    expect { drop(3) }.to output.to_stderr
    drop(4)

    expect { reporter.warn_dropped_total }.to output(
      "[buildkite-test_collector] OpenTelemetry dropped 7 child span(s) so far this run; " \
        "test.execution results are unaffected.\n"
    ).to_stderr
    expect { reporter.warn_dropped_total }.not_to output.to_stderr
  end
end
