# frozen_string_literal: true

require "open3"
require "json"
require "opentelemetry/sdk"
require "opentelemetry/trace/propagation/trace_context"

RSpec.describe Buildkite::TestCollector::OTel::RackMiddleware do
  it "forwards real Capybara server requests only to their owning example, preserving HTTP parentage" do
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, "-Ilib", "spec/fixtures/rack_context.rb")
    # The first example deliberately fails in teardown, after /late started.
    expect(status.exitstatus).to eq(1), stderr
    result = JSON.parse(stdout.lines.last)
    spans = result.fetch("exported")
    work = spans.select { |span| span["name"].start_with?("work ") }
    expect(work.map { |span| span["name"] }).to contain_exactly("work /traced", "work /late", "work /second", "work /direct")

    roots = spans.select { |span| span["name"] == "test.execution" }
    expect(roots.size).to eq(3)
    first = roots.find { |span| span["attributes"]["test.case.name"].include?("sends traced") }
    second = roots.find { |span| span["attributes"]["test.case.name"].include?("does not claim") }
    third = roots.find { |span| span["attributes"]["test.case.name"].include?("propagates a phase") }
    expect(first["attributes"]["test.case.result.status"]).to eq("fail")
    expect(second["attributes"]["test.case.result.status"]).to eq("pass")
    expect(third["attributes"]["test.case.result.status"]).to eq("pass")
    work.each do |span|
      root = { "work /second" => second, "work /direct" => third }.fetch(span["name"], first)
      expect(span["trace"]).to eq(root["trace"])
      server = spans.find { |candidate| candidate["id"] == span["parent"] }
      expect(server).not_to be_nil
      expect(server["kind"]).to eq("SPAN_KIND_SERVER")
      parent = spans.find { |candidate| candidate["id"] == server["parent"] }
      expect(parent).not_to be_nil
      # A normal HTTP request nests under its client span, not directly under
      # the test root. Explicit propagation without a client uses test.body.
      if ["work /late", "work /direct"].include?(span["name"])
        expect(parent["name"]).to eq("test.body")
      else
        expect(parent["kind"]).to eq("SPAN_KIND_CLIENT")
      end
    end
    late = work.find { |span| span["name"] == "work /late" }
    expect(late["tracestate"]).to eq("vendor=value")
    # The suite-owned exporter still receives unrelated requests, unchanged.
    customer_work = result.fetch("customer").select { |span| span["name"].start_with?("work ") }
    expect(customer_work.map { |span| span["name"] }).to include("work /stale", "work /unknown", "work /malformed", "work /headerless")
    expect(customer_work).to include(*work.map { |span| span.slice("name", "trace", "id", "parent") })
  end

  it "restores context on errors and forgets completed, shutdown, and inherited test registrations" do
    otel = Buildkite::TestCollector::OTel
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    otel.instance_variable_set(:@tracer, provider.tracer("rack-lifecycle"))
    root = otel.start_test_span(test: double(otel_attributes: {}))
    env = { "HTTP_TRACEPARENT" => "00-#{root.context.hex_trace_id}-#{root.context.hex_span_id}-01" }
    app = described_class.new(->(_) { [otel.current_test_span, OpenTelemetry::Context.current] })

    otel.with_test_span(root) do
      Thread.new do
        ambient = OpenTelemetry::Context.current
        expect(app.call(env).first).to equal(root)
        raising = described_class.new(->(_) { raise "app error" })
        expect { raising.call(env) }.to raise_error("app error")
        expect(OpenTelemetry::Context.current).to equal(ambient)
      end.value
      if Process.respond_to?(:fork)
        pid = fork { exit!(Thread.new { app.call(env).first.nil? }.value ? 0 : 1) }
        expect(Process.wait2(pid).last).to be_success
      end
    end
    # RSpec reports the final result later: the root is still recording but
    # requests arriving after the example's scope have already lost ownership.
    expect(root).to be_recording
    expect(app.call(env).first).to be_nil
    expect { otel.with_test_span(root) { raise "example error" } }.to raise_error("example error")
    expect(app.call(env).first).to be_nil
    otel.with_test_span(root) do
      otel.shutdown
      otel.instance_variable_set(:@tracer, provider.tracer("reconfigured"))
      expect(Thread.new { app.call(env).first }.value).to be_nil
    end
  ensure
    root&.finish
    otel&.shutdown
    provider&.shutdown
  end
end
