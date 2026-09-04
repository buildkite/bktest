# frozen_string_literal: true

require "open3"
require "opentelemetry/sdk"
require "opentelemetry/exporter/otlp"
require "opentelemetry/trace/propagation/trace_context"

RSpec.describe Buildkite::TestCollector::OTel do
  # A passed test that describes nothing, for specs about the span itself.
  def execution_test
    double("test", otel_attributes: {}, otel_result: "passed")
  end

  it "starts the test span as a trace root and links it to the Agent job trace" do
    exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    processor = OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    provider.add_span_processor(processor)
    tracer = provider.tracer("correlation-test")
    job_trace_id = "0af7651916cd43dd8448eb211c80319c"
    job_span_id = "b7ad6b7169203331"
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("TRACEPARENT")
      .and_return("00-#{job_trace_id}-#{job_span_id}-01")
    allow(ENV).to receive(:[]).with("TRACESTATE").and_return("vendor=value")

    described_class.instance_variable_set(:@tracer, tracer)

    tracer.in_span("ambient") do
      execution_span = described_class.start_test_span(test: execution_test)
      described_class.with_test_span(execution_span) do
        tracer.in_span("child") { nil }
      end
      described_class.finish_test_span(execution_span, test: execution_test)
    end
    provider.force_flush

    execution_span = exporter.finished_spans.find { |span| span.name == "test.execution" }
    child_span = exporter.finished_spans.find { |span| span.name == "child" }
    ambient_span = exporter.finished_spans.find { |span| span.name == "ambient" }

    expect(execution_span.parent_span_id).to eq(OpenTelemetry::Trace::INVALID_SPAN_ID)
    expect(execution_span.links.length).to eq(1)
    expect(execution_span.links.first.span_context.hex_trace_id).to eq(job_trace_id)
    expect(execution_span.links.first.span_context.hex_span_id).to eq(job_span_id)
    expect(execution_span.links.first.span_context.tracestate.to_s).to eq("vendor=value")
    expect(child_span.parent_span_id).to eq(execution_span.span_id)
    expect(child_span.trace_id).to eq(execution_span.trace_id)
    expect(ambient_span.trace_id).not_to eq(execution_span.trace_id)
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end

  it "skips missing or malformed Agent trace context" do
    exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    processor = OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    provider.add_span_processor(processor)
    described_class.instance_variable_set(:@tracer, provider.tracer("invalid-link-test"))
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("TRACEPARENT").and_return(nil, "not-a-traceparent")
    allow(ENV).to receive(:[]).with("TRACESTATE").and_return(nil)

    2.times do
      span = described_class.start_test_span(test: execution_test)
      described_class.finish_test_span(span, test: execution_test)
    end
    provider.force_flush

    expect(exporter.finished_spans.map(&:links)).to all(be_empty)
    expect(exporter.finished_spans.map(&:parent_span_id))
      .to all(eq(OpenTelemetry::Trace::INVALID_SPAN_ID))
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end

  it "fails open and reports a missing result when starting a span fails" do
    tracer = double("OpenTelemetry tracer")
    allow(tracer).to receive(:start_span).and_raise("start failed")
    described_class.instance_variable_set(:@tracer, tracer)
    reporter = described_class.const_get(:TestSpanMetricsReporter, false).new
    described_class.instance_variable_set(:@test_span_metrics_reporter, reporter)

    expect { expect(described_class.start_test_span(test: execution_test)).to be_nil }
      .to output(/TEST RESULTS MISSING: .* dropped 1 test\.execution span\(s\) \(could not start span: RuntimeError: start failed\)/).to_stderr
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    described_class.instance_variable_set(:@test_span_metrics_reporter, nil)
  end

  it "records what the test was and how it went" do
    span_class = Struct.new(:attributes, :status, :ended) do
      def set_attribute(key, value)
        attributes[key] = value
      end

      def finish
        self.ended = true
      end
    end
    described = Struct.new(:otel_attributes, :otel_result) do
      def otel_failure_reason
        nil
      end

      def otel_exception_events
        []
      end
    end

    failed = span_class.new({})
    described_class.finish_test_span(
      failed,
      test: described.new({ "test.case.name" => "adds up", "code.line.number" => nil }, "failed"),
    )
    skipped = span_class.new({})
    described_class.finish_test_span(skipped, test: described.new({}, "skipped"))

    expect(failed.attributes).to eq(
      "test.case.name" => "adds up",
      "test.case.result.status" => "fail",
    )
    expect(failed.status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    expect(failed.ended).to be(true)
    expect(skipped.attributes.fetch("test.case.result.status")).to eq("skipped")
    expect(skipped.status).to be_nil
    expect(skipped.ended).to be(true)
  end

  it "marks every test span as the submission, even one whose test describes nothing" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    described_class.instance_variable_set(:@tracer, provider.tracer("via-marker-test"))

    span = described_class.start_test_span(test: execution_test)

    expect(span.to_span_data.attributes).to eq(
      "buildkite.execution.via" => "otlp",
      "test.case.result.status" => "unset",
    )
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end

  it "reserves synthesis attributes before descriptive fields, run metadata, and tags" do
    exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    processor = OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
    limits = OpenTelemetry::SDK::Trace::SpanLimits.new(attribute_count_limit: 3)
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new(span_limits: limits)
    provider.add_span_processor(processor)
    described_class.instance_variable_set(:@tracer, provider.tracer("attribute-priority-test"))
    described_class.instance_variable_set(
      :@run_attributes,
      {
        "buildkite.run_key" => "run-123",
        "buildkite.message" => "optional metadata",
        "buildkite.tag.configured" => "optional tag",
      },
    )
    test = double(
      "trace",
      otel_attributes: {
        "buildkite.test.scope" => "Math",
        "buildkite.test.name" => "adds numbers",
        "test.case.name" => "Math adds numbers",
        "test.suite.name" => "Math",
        "code.file.path" => "spec/math_spec.rb",
        "code.line.number" => 12,
        "buildkite.test.execution.external_id" => "execution-123",
        "buildkite.tag.execution" => "optional tag",
      },
      otel_result: "passed",
    )

    span = described_class.start_test_span(test: test)
    described_class.with_test_span(span) do
      OpenTelemetry::Trace.current_span.set_attribute("custom.attribute", "optional")
    end
    described_class.finish_test_span(span, test: test)
    provider.force_flush

    expect(exporter.finished_spans.fetch(0).attributes).to eq(
      "buildkite.execution.via" => "otlp",
      "buildkite.run_key" => "run-123",
      "test.case.result.status" => "pass",
    )
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    described_class.instance_variable_set(:@run_attributes, nil)
    provider&.shutdown
  end

  it "falls back to the SDK's clock when the captured end precedes the span's start" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    described_class.instance_variable_set(:@tracer, provider.tracer("clock-step-test"))

    span = described_class.start_test_span(test: execution_test)
    stepped_back = described_class.current_timestamp - 60
    described_class.finish_test_span(span, test: execution_test, end_timestamp: stepped_back)

    data = span.to_span_data
    expect(data.end_timestamp).to be >= data.start_timestamp
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end

  it "finishes the span even when the test cannot be described" do
    exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    processor = OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    provider.add_span_processor(processor)
    described_class.instance_variable_set(:@tracer, provider.tracer("description-failure-test"))
    test = double("test", otel_result: "passed")
    calls = 0
    allow(test).to receive(:otel_attributes) do
      calls += 1
      raise "no metadata for you" if calls > 1

      {}
    end

    span = described_class.start_test_span(test: test)
    expect { described_class.finish_test_span(span, test: test) }
      .to output(/Could not describe OpenTelemetry test span/).to_stderr
    provider.force_flush

    expect(exporter.finished_spans.fetch(0).attributes).to include(
      "buildkite.execution.via" => "otlp",
      "test.case.result.status" => "pass",
    )
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end

  it "asks nothing of the test when there is no span" do
    test = double("test")

    expect { described_class.finish_test_span(nil, test: test) }.not_to raise_error
  end

  it "deactivates children, then shuts down test spans and children against one deadline" do
    success = OpenTelemetry::SDK::Trace::Export::SUCCESS
    forwarder = double("execution child forwarder")
    test_span_provider = double("execution provider")
    child_processor = double("execution child processor")
    described_class.instance_variable_set(:@child_span_forwarder, forwarder)
    described_class.instance_variable_set(:@test_span_provider, test_span_provider)
    described_class.instance_variable_set(:@child_span_processor, child_processor)
    allow(Process).to receive(:clock_gettime)
      .with(Process::CLOCK_MONOTONIC)
      .and_return(10.0, 12.0, 13.0)

    expect(forwarder).to receive(:shutdown).ordered.and_return(success)
    expect(test_span_provider).to receive(:shutdown).with(timeout: 28.0).ordered.and_return(success)
    expect(child_processor).to receive(:shutdown).with(timeout: 27.0).ordered.and_return(success)

    described_class.shutdown
  end

  it "flushes test spans and children against one shared deadline" do
    test_span_provider = double("execution provider")
    child_processor = double("execution child processor")
    described_class.instance_variable_set(:@test_span_provider, test_span_provider)
    described_class.instance_variable_set(:@child_span_processor, child_processor)
    allow(Process).to receive(:clock_gettime)
      .with(Process::CLOCK_MONOTONIC)
      .and_return(10.0, 12.0, 13.0)

    expect(test_span_provider).to receive(:force_flush).with(timeout: 28.0).ordered
    expect(child_processor).to receive(:force_flush).with(timeout: 27.0).ordered

    described_class.force_flush
  ensure
    described_class.instance_variable_set(:@test_span_provider, nil)
    described_class.instance_variable_set(:@child_span_processor, nil)
  end

  it "still flushes children when the test span flush fails" do
    test_span_provider = double("execution provider")
    child_processor = double("execution child processor")
    allow(test_span_provider).to receive(:force_flush).and_raise("test span flush failed")
    described_class.instance_variable_set(:@test_span_provider, test_span_provider)
    described_class.instance_variable_set(:@child_span_processor, child_processor)

    expect(child_processor).to receive(:force_flush)
    expect { described_class.force_flush }
      .to output(/Could not flush OpenTelemetry spans.*test span flush failed/).to_stderr
  ensure
    described_class.instance_variable_set(:@test_span_provider, nil)
    described_class.instance_variable_set(:@child_span_processor, nil)
  end

  it "attempts child shutdown when test span shutdown fails" do
    test_span_provider = double("execution provider")
    child_processor = spy(
      "execution child processor",
      shutdown: OpenTelemetry::SDK::Trace::Export::SUCCESS,
    )
    allow(test_span_provider).to receive(:shutdown).and_raise("test span shutdown failed")
    described_class.instance_variable_set(:@test_span_provider, test_span_provider)
    described_class.instance_variable_set(:@child_span_processor, child_processor)

    expect { described_class.shutdown }
      .to output(/Could not shut down OpenTelemetry span export: RuntimeError: test span shutdown failed/)
      .to_stderr
    expect(child_processor).to have_received(:shutdown).with(timeout: be_between(0, 30))
    expect(described_class).not_to be_enabled
  end

  it "fails open when an OpenTelemetry dependency cannot be loaded" do
    allow(described_class).to receive(:require).and_call_original
    allow(described_class).to receive(:require)
      .with("opentelemetry/sdk")
      .and_raise(LoadError, "cannot load OpenTelemetry SDK")

    expect do
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        run_env: { "key" => "run-123" },
      )
    end.to output(/OpenTelemetry span export disabled: LoadError/).to_stderr
    expect(described_class).not_to be_enabled
  end

  it "registers a process-lifetime at_exit shutdown once, on successful configure" do
    suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(suite_provider)
    allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) do
      OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    end
    previous = described_class.instance_variable_get(:@shutdown_at_exit_registered)
    described_class.instance_variable_set(:@shutdown_at_exit_registered, nil)
    registrations = 0
    allow(described_class).to receive(:at_exit) { registrations += 1 }

    # A failed configure leaves nothing to shut down at exit.
    expect do
      described_class.configure!(instrumentations: [:all])
    end.to output(/OpenTelemetry span export disabled/).to_stderr
    expect(registrations).to eq(0)

    described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    expect(registrations).to eq(1)

    # Reconfiguring after an explicit shutdown must not stack another handler.
    described_class.shutdown
    described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    expect(registrations).to eq(1)
  ensure
    described_class.shutdown
    described_class.instance_variable_set(:@shutdown_at_exit_registered, previous)
    suite_provider&.shutdown
  end

  it "shuts down the execution processor when private provider setup fails" do
    test_span_processor = spy(
      "OpenTelemetry execution processor",
      shutdown: OpenTelemetry::SDK::Trace::Export::SUCCESS,
    )
    allow(described_class).to receive(:batch_processor).and_return(test_span_processor)
    allow(OpenTelemetry::SDK::Trace::TracerProvider)
      .to receive(:new)
      .and_raise(ArgumentError, "invalid private provider configuration")

    expect do
      described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    end.to output(/OpenTelemetry span export disabled: ArgumentError/).to_stderr
    expect(test_span_processor).to have_received(:shutdown).with(timeout: 0)
    expect(described_class).not_to be_enabled
  end

  it "keeps test span export enabled when child processor setup fails" do
    suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(suite_provider)
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    calls = 0
    allow(described_class).to receive(:batch_processor) do |_endpoint, _headers, options = {}|
      calls += 1
      raise ArgumentError, "invalid child queue configuration" if calls == 2

      OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(root_exporter, **options)
    end

    expect do
      described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    end.to output(
      /OpenTelemetry child span export disabled: ArgumentError: invalid child queue configuration; test.execution export remains enabled/
    ).to_stderr

    execution_span = described_class.start_test_span(test: execution_test)
    described_class.finish_test_span(execution_span, test: execution_test)
    described_class.instance_variable_get(:@test_span_provider).force_flush

    expect(described_class).to be_enabled
    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
  ensure
    described_class.shutdown
    suite_provider&.shutdown
  end

  it "makes a partially attached child forwarder inert and stops its worker" do
    success = OpenTelemetry::SDK::Trace::Export::SUCCESS
    test_span_processor = spy(
      "execution processor",
      on_start: nil,
      on_finish: nil,
      shutdown: success,
    )
    child_processor = spy(
      "execution child processor",
      on_finish: nil,
      shutdown: success,
    )
    suite_provider = double("suite provider")
    forwarder = nil
    allow(suite_provider).to receive(:add_span_processor) do |attached|
      forwarder = attached
      raise "attachment failed"
    end
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(suite_provider)
    allow(described_class).to receive(:batch_processor)
      .and_return(test_span_processor, child_processor)

    expect do
      described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    end.to output(
      /OpenTelemetry child span export disabled: RuntimeError: attachment failed; test.execution export remains enabled/
    ).to_stderr

    trace_id = "\1" * 16
    context = OpenTelemetry::Context.empty.set_value(
      described_class.send(:test_span_context_key),
      trace_id,
    )
    span = double("suite span", context: double("span context", trace_id: trace_id))
    forwarder.on_start(span, context)
    forwarder.on_finish(span)

    expect(described_class).to be_enabled
    expect(child_processor).to have_received(:shutdown).with(timeout: 0).once
    expect(child_processor).not_to have_received(:on_finish)

    described_class.shutdown
    expect(test_span_processor).to have_received(:shutdown).once
    expect(child_processor).to have_received(:shutdown).once
  ensure
    described_class.shutdown
  end

  it "cleans up a child worker when the SDK fails to install its provider" do
    success = OpenTelemetry::SDK::Trace::Export::SUCCESS
    test_span_processor = spy(
      "execution processor",
      on_start: nil,
      on_finish: nil,
      shutdown: success,
    )
    child_processor = spy(
      "execution child processor",
      on_finish: nil,
      shutdown: success,
    )
    proxy_provider = OpenTelemetry::Internal::ProxyTracerProvider.new
    config = double("OpenTelemetry SDK configuration")
    forwarder = nil
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(proxy_provider)
    allow(config).to receive(:id_generator=)
    allow(config).to receive(:add_span_processor) { |attached| forwarder = attached }
    allow(OpenTelemetry::SDK).to receive(:configure) do |&block|
      block.call(config)
    end
    allow(config).to receive(:resource=)
    allow(described_class).to receive(:batch_processor)
      .and_return(test_span_processor, child_processor)

    expect do
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        instrumentations: [],
      )
    end.to output(
      /OpenTelemetry child span export disabled: RuntimeError: OpenTelemetry SDK did not install a tracer provider; test.execution export remains enabled/
    ).to_stderr

    trace_id = "\1" * 16
    context = OpenTelemetry::Context.empty.set_value(
      described_class.send(:test_span_context_key),
      trace_id,
    )
    span = double("collector span", context: double("span context", trace_id: trace_id))
    forwarder.on_start(span, context)
    forwarder.on_finish(span)

    expect(described_class).to be_enabled
    expect(child_processor).to have_received(:shutdown).with(timeout: 0).once
    expect(child_processor).not_to have_received(:on_finish)

    described_class.shutdown
    expect(test_span_processor).to have_received(:shutdown).once
    expect(child_processor).to have_received(:shutdown).once
  ensure
    described_class.shutdown
  end

  it "uses process-safe IDs and installs registered instrumentation for collector-managed children" do
    child_processor = spy(
      "execution child processor",
      shutdown: OpenTelemetry::SDK::Trace::Export::SUCCESS,
    )
    provider = OpenTelemetry::Internal::ProxyTracerProvider.new
    configured_provider = double("configured provider")
    config = double("OpenTelemetry SDK configuration")
    generator = described_class.const_get(:SecureRandomIdGenerator, false)
    allow(OpenTelemetry).to receive(:tracer_provider)
      .and_return(provider, configured_provider)
    allow(OpenTelemetry::SDK).to receive(:configure).and_yield(config)
    allow(config).to receive(:add_span_processor)
    allow(config).to receive(:id_generator=)
    allow(config).to receive(:use_all)
    allow(described_class).to receive(:batch_processor).and_return(child_processor)
    allow(config).to receive(:resource=)

    described_class.send(
      :configure_child_export,
      "https://example.invalid/v1/traces",
      {},
      nil,
      OpenTelemetry::SDK::Resources::Resource.create({}),
    )

    expect(config).to have_received(:id_generator=).with(generator)
    expect(config).to have_received(:add_span_processor)
      .with(an_instance_of(described_class.const_get(:ChildSpanForwarder, false)))
    expect(config).to have_received(:use_all).once
  ensure
    described_class.shutdown
  end

  it "does not install registered instrumentation with an empty selection" do
    provider = OpenTelemetry::Internal::ProxyTracerProvider.new
    configured_provider = double("configured provider")
    config = double("OpenTelemetry SDK configuration")
    child_processor = spy(
      "execution child processor",
      shutdown: OpenTelemetry::SDK::Trace::Export::SUCCESS,
    )
    allow(OpenTelemetry).to receive(:tracer_provider)
      .and_return(provider, configured_provider)
    allow(OpenTelemetry::SDK).to receive(:configure).and_yield(config)
    allow(config).to receive(:id_generator=)
    allow(config).to receive(:add_span_processor)
    allow(config).to receive(:use_all)
    allow(described_class).to receive(:batch_processor).and_return(child_processor)
    allow(config).to receive(:resource=)

    described_class.send(
      :configure_child_export,
      "https://example.invalid/v1/traces",
      {},
      [],
      OpenTelemetry::SDK::Resources::Resource.create({}),
    )

    expect(config).not_to have_received(:use_all)
  ensure
    described_class.shutdown
  end

  it "fails open for instrumentation selections reserved for a later release" do
    expect do
      described_class.configure!(instrumentations: [:all])
    end.to output(
      /OpenTelemetry span export disabled: ArgumentError: otel_instrumentations must be omitted or \[\]/
    ).to_stderr

    expect(described_class).not_to be_enabled
  end

  it "sends the run key and token as request headers" do
    headers = described_class.send(:request_headers, { "key" => "test-run-id" }, "suite-token")

    expect(headers).to eq(
      "Buildkite-Tests-Run-Key" => "test-run-id",
      "Authorization" => %(Token token="suite-token"),
    )
  end

  it "gives trace-specific OTLP headers precedence over generic and collector headers" do
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_TRACES_HEADERS")
      .and_return(
        "authorization=Bearer%20relay-token,buildkite-tests-run-key=relay-run,x-extra=hello%20world"
      )
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_HEADERS")
      .and_return("authorization=Bearer%20generic-token")

    headers = described_class.send(:request_headers, { "key" => "test-run-id" }, "suite-token")

    expect(headers).to eq(
      "authorization" => "Bearer relay-token",
      "buildkite-tests-run-key" => "relay-run",
      "x-extra" => "hello world",
    )
  end

  it "uses generic OTLP headers when trace-specific headers are empty" do
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_TRACES_HEADERS").and_return("")
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_HEADERS")
      .and_return("Authorization=Bearer%20generic-token")

    headers = described_class.send(:request_headers, { "key" => "test-run-id" }, "suite-token")

    expect(headers["Authorization"]).to eq("Bearer generic-token")
  end

  it "uses collector headers when both standard OTLP header variables are empty" do
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_TRACES_HEADERS").and_return("")
    allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_HEADERS").and_return("")

    headers = described_class.send(:request_headers, { "key" => "test-run-id" }, "suite-token")

    expect(headers).to eq(
      "Buildkite-Tests-Run-Key" => "test-run-id",
      "Authorization" => %(Token token="suite-token"),
    )
  end

  it "uses an AlwaysOn sampler, process-safe random IDs, and the producer resource for test spans" do
    processor = spy(
      "execution processor",
      shutdown: OpenTelemetry::SDK::Trace::Export::SUCCESS,
    )
    generator = described_class.const_get(:SecureRandomIdGenerator, false)
    resource = described_class.send(:producer_resource, {})
    allow(described_class).to receive(:batch_processor).and_return(processor)

    test_span_provider = described_class.send(
      :build_test_span_provider,
      "https://example.invalid/v1/traces",
      {},
      resource,
    )

    expect(test_span_provider.id_generator).to equal(generator)
    expect(test_span_provider.sampler).to equal(OpenTelemetry::SDK::Trace::Samplers::ALWAYS_ON)
    expect(test_span_provider.resource).to equal(resource)
  ensure
    test_span_provider&.shutdown
  end

  it "adds configure-level tags to test spans" do
    attributes = described_class.send(:run_attributes, {}, { "team" => "platform" })

    expect(attributes).to include(
      "buildkite.tag.team" => "platform",
    )
  end

  it "exports test spans privately and only forwards their children" do
    suite_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(suite_exporter).to receive(:shutdown).and_call_original
    suite_resource = OpenTelemetry::SDK::Resources::Resource.create(
      "service.name" => "my-suite",
    )
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new(resource: suite_resource)
    provider.add_span_processor(
      OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(suite_exporter)
    )
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(provider)

    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    child_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(root_exporter).to receive(:shutdown)
      .and_return(OpenTelemetry::SDK::Trace::Export::SUCCESS)
    allow(child_exporter).to receive(:shutdown)
      .and_return(OpenTelemetry::SDK::Trace::Export::SUCCESS)
    allow(OpenTelemetry::Exporter::OTLP::Exporter)
      .to receive(:new)
      .and_return(root_exporter, child_exporter)
    test_span_reporter = described_class.const_get(:TestSpanMetricsReporter, false)
    expect(OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor)
      .to receive(:new)
      .with(
        root_exporter,
        max_queue_size: described_class::TEST_SPAN_MAX_QUEUE_SIZE,
        max_export_batch_size: described_class::TEST_SPAN_MAX_EXPORT_BATCH_SIZE,
        schedule_delay: described_class::TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS,
        metrics_reporter: an_instance_of(test_span_reporter),
      )
      .ordered
      .and_call_original
    # Children keep the SDK defaults, including its no-op metrics reporter.
    expect(OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor)
      .to receive(:new)
      .with(child_exporter, metrics_reporter: nil)
      .ordered
      .and_call_original
    described_class.configure!(endpoint: "https://example.invalid/v1/traces")

    tracer = provider.tracer("suite")
    tracer.in_span("before-execution") { nil }
    execution_span = described_class.start_test_span(test: execution_test)
    late_child = nil
    described_class.with_test_span(execution_span) do
      tracer.in_span("child") { nil }
      detached = tracer.start_span("detached", with_parent: OpenTelemetry::Context.empty)
      OpenTelemetry::Trace.with_span(detached) do
        tracer.in_span("detached-child") { nil }
      end
      detached.finish
      late_child = tracer.start_span("late-child")
    end
    described_class.finish_test_span(execution_span, test: execution_test)
    late_child.finish
    tracer.in_span("after-execution") { nil }
    described_class.instance_variable_get(:@test_span_provider).force_flush
    described_class.instance_variable_get(:@child_span_processor).force_flush
    provider.force_flush

    expect(OpenTelemetry.tracer_provider).to equal(provider)
    expect(execution_span.context.trace_flags).to be_sampled
    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
    expect(child_exporter.finished_spans.map(&:name)).to contain_exactly("child", "late-child")
    expect(suite_exporter.finished_spans.map(&:name))
      .to contain_exactly(
        "before-execution",
        "child",
        "detached",
        "detached-child",
        "late-child",
        "after-execution",
      )
    exported_root = root_exporter.finished_spans.fetch(0)
    exported_child = child_exporter.finished_spans.find { |span| span.name == "child" }
    expect(exported_child.trace_id).to eq(exported_root.trace_id)
    expect(exported_child.parent_span_id).to eq(exported_root.span_id)
    expect(exported_root.resource.attribute_enumerator.to_h).to include(
      "service.name" => "unknown_service",
    )
    expect(exported_child.resource).to equal(suite_resource)

    described_class.shutdown
    tracer.in_span("after-shutdown") { nil }
    provider.force_flush

    expect(child_exporter.finished_spans.map(&:name)).not_to include("after-shutdown")
    expect(suite_exporter.finished_spans.map(&:name)).to include("after-shutdown")
    expect(suite_exporter).not_to have_received(:shutdown)
  ensure
    described_class.shutdown
    provider&.shutdown
  end

  it "shares one metrics reporter between the test span exporter and its processor" do
    test_span_reporter = described_class.const_get(:TestSpanMetricsReporter, false)
    exporter_reporters = []
    allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) do |**options|
      exporter_reporters << options[:metrics_reporter]
      OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    end
    processor_reporters = []
    allow(OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor)
      .to receive(:new).and_wrap_original do |original, exporter, **options|
        processor_reporters << options[:metrics_reporter]
        original.call(exporter, **options)
      end

    described_class.configure!(endpoint: "https://example.invalid/v1/traces", instrumentations: [])

    # Test spans are built first; children keep the SDK's default (no-op) reporter.
    expect(exporter_reporters.first).to be_a(test_span_reporter)
    expect(processor_reporters.first).to equal(exporter_reporters.first)
    expect(exporter_reporters.drop(1)).to all(be_nil)
  ensure
    described_class.shutdown
  end

  it "reports each suite run's total dropped test spans when every export fails" do
    failing_exporter = Class.new do
      def export(_spans, timeout: nil) = OpenTelemetry::SDK::Trace::Export::FAILURE
      def force_flush(timeout: nil) = OpenTelemetry::SDK::Trace::Export::SUCCESS
      def shutdown(timeout: nil) = OpenTelemetry::SDK::Trace::Export::SUCCESS
    end.new
    allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new).and_return(failing_exporter)
    allow(OpenTelemetry).to receive(:handle_error)
    described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    processor = described_class.instance_variable_get(:@test_span_provider)
      .instance_variable_get(:@span_processors).first

    record_execution = lambda do
      test = execution_test
      described_class.finish_test_span(described_class.start_test_span(test: test), test: test)
    end

    # Exporting one test span fails and warns inline; the suite-end flush then
    # reports the run's total, which the inline warning did not cover.
    record_execution.call
    expect { processor.force_flush }
      .to output(/dropped 1 test\.execution span\(s\) \(export-failure\)/).to_stderr
    2.times { record_execution.call }
    expect { described_class.force_flush }
      .to output(/dropped 3 test\.execution span\(s\) so far this run/).to_stderr

    # A warm worker's next suite run gets its own inline warning; shutdown
    # has nothing new to add.
    record_execution.call
    expect { described_class.force_flush }
      .to output(/dropped 1 test\.execution span\(s\) \(export-failure\)/).to_stderr
    expect { described_class.shutdown }.not_to output.to_stderr
  ensure
    described_class.shutdown
  end

  it "filters child spans without filtering test spans" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(provider)
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    child_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(OpenTelemetry::Exporter::OTLP::Exporter)
      .to receive(:new)
      .and_return(root_exporter, child_exporter)
    described_class.configure!(
      endpoint: "https://example.invalid/v1/traces",
      span_filter: ->(span) { span.name == "kept-child" },
    )

    execution_span = described_class.start_test_span(test: execution_test)
    described_class.with_test_span(execution_span) do
      tracer = provider.tracer("suite")
      tracer.in_span("kept-child") { nil }
      tracer.in_span("dropped-child") { nil }
    end
    described_class.finish_test_span(execution_span, test: execution_test)
    described_class.instance_variable_get(:@test_span_provider).force_flush
    described_class.instance_variable_get(:@child_span_processor).force_flush

    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
    expect(child_exporter.finished_spans.map(&:name)).to contain_exactly("kept-child")
  ensure
    described_class.shutdown
    provider&.shutdown
  end

  it "leaves instrumentation alone when the suite owns its provider" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(provider)
    allow(OpenTelemetry::SDK).to receive(:configure)
    allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) do
      OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    end

    expect do
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        instrumentations: [],
      )
    end.to output(
      /instrumentation selection ignored because the test suite already configured OpenTelemetry: \[\]/
    ).to_stderr

    expect(described_class).to be_enabled
    expect(OpenTelemetry::SDK).not_to have_received(:configure)
  ensure
    described_class.shutdown
    provider&.shutdown
  end

  it "exports test spans when the suite's sampler drops child spans" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
      sampler: OpenTelemetry::SDK::Trace::Samplers::ALWAYS_OFF,
    )
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(provider)
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    child_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(OpenTelemetry::Exporter::OTLP::Exporter)
      .to receive(:new)
      .and_return(root_exporter, child_exporter)
    described_class.configure!(endpoint: "https://example.invalid/v1/traces")

    execution_span = described_class.start_test_span(test: execution_test)
    described_class.with_test_span(execution_span) do
      provider.tracer("suite").in_span("sampled-out-child") { nil }
    end
    described_class.finish_test_span(execution_span, test: execution_test)
    described_class.instance_variable_get(:@test_span_provider).force_flush
    described_class.instance_variable_get(:@child_span_processor).force_flush

    expect(execution_span.context.trace_flags).to be_sampled
    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
    expect(child_exporter.finished_spans).to be_empty
  ensure
    described_class.shutdown
    provider&.shutdown
  end

  it "keeps private test span export alive if the suite shuts down its provider" do
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(provider)
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    child_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(OpenTelemetry::Exporter::OTLP::Exporter)
      .to receive(:new)
      .and_return(root_exporter, child_exporter)
    described_class.configure!(endpoint: "https://example.invalid/v1/traces")

    provider.shutdown
    provider = nil
    execution_span = described_class.start_test_span(test: execution_test)
    described_class.finish_test_span(execution_span, test: execution_test)
    described_class.instance_variable_get(:@test_span_provider).force_flush

    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
  ensure
    described_class.shutdown
    provider&.shutdown
  end

  describe "token refresh" do
    def exporter_authorization_headers
      described_class.instance_variable_get(:@exporters).map do |exporter|
        exporter.instance_variable_get(:@headers).find do |key, _|
          key.casecmp?("Authorization")
        end&.last
      end
    end

    it "refreshes the Authorization header when reconfigured with a new token" do
      original = OpenTelemetry.tracer_provider
      suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      OpenTelemetry.tracer_provider = suite_provider

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: "before-refresh",
        run_env: { "key" => "run-123" },
      )
      # Both the execution exporter and the child exporter carry the token.
      expect(exporter_authorization_headers).to eq(['Token token="before-refresh"'] * 2)
      provider_before = described_class.instance_variable_get(:@test_span_provider)

      # A warm worker re-running configure with a refreshed (e.g. expiring
      # OIDC) token: the live exporters must adopt it, without rebuilding.
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: "after-refresh",
        run_env: { "key" => "run-123" },
      )

      expect(exporter_authorization_headers).to eq(['Token token="after-refresh"'] * 2)
      expect(described_class.instance_variable_get(:@test_span_provider)).to equal(provider_before)
    ensure
      described_class.shutdown
      suite_provider&.shutdown
      OpenTelemetry.tracer_provider = original
    end

    it "does not replace standard OTLP authorization when the collector token changes" do
      original = OpenTelemetry.tracer_provider
      suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      OpenTelemetry.tracer_provider = suite_provider
      allow(ENV).to receive(:[]).and_call_original
      allow(ENV).to receive(:[]).with("OTEL_EXPORTER_OTLP_TRACES_HEADERS")
        .and_return("authorization=Bearer%20relay-token")

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: "before-refresh",
        run_env: { "key" => "run-123" },
      )
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: "after-refresh",
        run_env: { "key" => "run-123" },
      )

      expect(exporter_authorization_headers).to eq(["Bearer relay-token"] * 2)
    ensure
      described_class.shutdown
      suite_provider&.shutdown
      OpenTelemetry.tracer_provider = original
    end

    it "warns when reconfigured with a different run key, keeping the original run" do
      original = OpenTelemetry.tracer_provider
      suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      OpenTelemetry.tracer_provider = suite_provider

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        run_env: { "key" => "run-one" },
      )

      expect {
        described_class.configure!(
          endpoint: "https://example.invalid/v1/traces",
          run_env: { "key" => "run-two" },
        )
      }.to output(/already configured for run "run-one".*requires a new process/m).to_stderr

      # Same run key, or none: not a new run, so no warning.
      expect {
        described_class.configure!(
          endpoint: "https://example.invalid/v1/traces",
          run_env: { "key" => "run-one" },
        )
        described_class.configure!(endpoint: "https://example.invalid/v1/traces")
      }.not_to output.to_stderr
    ensure
      described_class.shutdown
      suite_provider&.shutdown
      OpenTelemetry.tracer_provider = original
    end

    it "keeps the current token when reconfigured without one" do
      original = OpenTelemetry.tracer_provider
      suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      OpenTelemetry.tracer_provider = suite_provider

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: "the-token",
        run_env: { "key" => "run-123" },
      )
      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        api_token: nil,
        run_env: { "key" => "run-123" },
      )

      expect(exporter_authorization_headers).to eq(['Token token="the-token"'] * 2)
    ensure
      described_class.shutdown
      suite_provider&.shutdown
      OpenTelemetry.tracer_provider = original
    end
  end

  describe "VCR exemption" do
    fake_request = Struct.new(:method, :uri)

    it "registers an ignore_request hook matching only a POST to the OTLP endpoint" do
      ignore_blocks = []
      vcr_config = double("VCR configuration")
      allow(vcr_config).to receive(:ignore_request) { |&block| ignore_blocks << block }
      fake_vcr = double("VCR")
      allow(fake_vcr).to receive(:configure).and_yield(vcr_config)
      stub_const("VCR", fake_vcr)
      allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) do
        OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      end

      described_class.configure!(endpoint: "https://tests-otlp.example.invalid/v1/traces")

      expect(ignore_blocks.length).to eq(1)
      ignored = ignore_blocks.first
      expect(ignored.call(fake_request.new(:post, "https://tests-otlp.example.invalid/v1/traces"))).to be true
      expect(ignored.call(fake_request.new(:post, "https://tests-otlp.example.invalid:443/v1/traces"))).to be true
      expect(ignored.call(fake_request.new(:get, "https://tests-otlp.example.invalid/v1/traces"))).to be false
      expect(ignored.call(fake_request.new(:post, "https://example.invalid/v1/traces"))).to be false
      expect(ignored.call(fake_request.new(:post, "https://tests-otlp.example.invalid/v1/uploads"))).to be false
      expect(ignored.call(fake_request.new(:post, "not a uri at all "))).to be false
    ensure
      described_class.shutdown
    end

    it "still configures span export when VCR refuses to cooperate" do
      fake_vcr = double("VCR")
      allow(fake_vcr).to receive(:configure).and_raise(RuntimeError, "cassette in use")
      stub_const("VCR", fake_vcr)
      allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) do
        OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      end

      expect {
        described_class.configure!(endpoint: "https://example.invalid/v1/traces")
      }.to output(/Could not exempt the OTLP endpoint from VCR/).to_stderr

      expect(described_class).to be_enabled
    ensure
      described_class.shutdown
    end
  end

  it "configures collector-managed children without suite OpenTelemetry" do
    script = <<~'RUBY'
      require "buildkite/test_collector"
      require "opentelemetry/sdk"
      require "opentelemetry/exporter/otlp"

      original_provider = OpenTelemetry.tracer_provider
      exporters = []
      OpenTelemetry::Exporter::OTLP::Exporter.singleton_class.define_method(:new) do |**_options|
        exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
        exporters << exporter
        exporter
      end

      Buildkite::TestCollector::OTel.configure!(
        endpoint: "https://example.invalid/v1/traces",
        instrumentations: [],
      )
      test = Struct.new(:otel_attributes, :otel_result).new({}, "passed")
      span = Buildkite::TestCollector::OTel.start_test_span(test: test)
      Buildkite::TestCollector::OTel.with_test_span(span) do
        OpenTelemetry.tracer_provider.tracer("application").in_span("child") { nil }
      end
      Buildkite::TestCollector::OTel.finish_test_span(span, test: test)
      Buildkite::TestCollector::OTel.instance_variable_get(:@test_span_provider).force_flush
      Buildkite::TestCollector::OTel.instance_variable_get(:@child_span_processor).force_flush

      root = exporters[0].finished_spans.find { |span| span.name == "test.execution" }
      child = exporters[1].finished_spans.find { |span| span.name == "child" }
      root_resource = root.resource.attribute_enumerator.to_h
      child_resource = child.resource.attribute_enumerator.to_h
      puts "global-provider-configured=#{!OpenTelemetry.tracer_provider.equal?(original_provider)}"
      puts "exporters=#{exporters.length}"
      puts "resources-match=#{root_resource == child_resource}"
      puts "root-resource-empty=#{root_resource.empty?}"
      puts exporters.flat_map { |exporter| exporter.finished_spans.map(&:name) }
      Buildkite::TestCollector::OTel.shutdown
    RUBY

    stdout, stderr, status = Open3.capture3(RbConfig.ruby, "-Ilib", "-e", script)

    expect(status).to be_success, stderr
    expect(stdout.lines.map(&:chomp)).to include(
      "global-provider-configured=true",
      "exporters=2",
      "resources-match=true",
      "root-resource-empty=false",
      "test.execution",
      "child",
    )
  end

  it "keeps test span export enabled with an incompatible suite provider" do
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    allow(OpenTelemetry).to receive(:tracer_provider).and_return(Object.new)
    allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new).and_return(root_exporter)

    expect do
      described_class.configure!(endpoint: "https://example.invalid/v1/traces")
    end.to output(
      /OpenTelemetry child span export disabled: RuntimeError: existing OpenTelemetry tracer provider does not support adding a span processor; test.execution export remains enabled/
    ).to_stderr

    execution_span = described_class.start_test_span(test: execution_test)
    described_class.finish_test_span(execution_span, test: execution_test)
    described_class.instance_variable_get(:@test_span_provider).force_flush

    expect(described_class).to be_enabled
    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
  ensure
    described_class.shutdown
  end

  describe "resource and execution attributes" do
    it "uses provider-native CI run IDs rather than the Test Engine run key" do
      allow(ENV).to receive(:[]).and_call_original
      allow(ENV).to receive(:[]).with("GITHUB_RUN_ID").and_return("github-123")
      allow(ENV).to receive(:[]).with("GITHUB_REPOSITORY").and_return("acme/payments")
      allow(ENV).to receive(:[]).with("CIRCLE_WORKFLOW_ID").and_return("circle-123")
      allow(ENV).to receive(:[]).with("CIRCLE_BUILD_URL").and_return("https://circle.example/workflow/123")

      github = described_class.send(
        :producer_resource,
        { "CI" => "github_actions", "key" => "test-engine-key" },
      ).attribute_enumerator.to_h
      circle = described_class.send(
        :producer_resource,
        { "CI" => "circleci", "key" => "test-engine-key" },
      ).attribute_enumerator.to_h

      expect(github).to include(
        "cicd.pipeline.run.id" => "github-123",
        "cicd.pipeline.run.url.full" => "https://github.com/acme/payments/actions/runs/github-123",
      )
      expect(circle).to include("cicd.pipeline.run.id" => "circle-123")
      expect(circle).not_to include("cicd.pipeline.run.url.full")
    end

    it "keeps configured URLs on the execution when they are not the CI resource URL" do
      allow(ENV).to receive(:[]).and_call_original
      allow(ENV).to receive(:[]).with("BUILDKITE_BUILD_ID").and_return("build-123")
      allow(ENV).to receive(:[]).with("BUILDKITE_BUILD_URL").and_return("https://buildkite.example/builds/123")

      buildkite = described_class.send(
        :run_attributes,
        { "CI" => "buildkite", "url" => "https://configured.example/run" },
        {},
      )
      generic = described_class.send(
        :run_attributes,
        { "CI" => "generic", "url" => "https://configured.example/generic-run" },
        {},
      )
      circle = described_class.send(
        :run_attributes,
        { "CI" => "circleci", "url" => "https://circle.example/jobs/123" },
        {},
      )

      expect(buildkite).to include("buildkite.run_url" => "https://configured.example/run")
      expect(generic).to include("buildkite.run_url" => "https://configured.example/generic-run")
      expect(circle).to include("buildkite.run_url" => "https://circle.example/jobs/123")
    end

    it "keeps producer identity on the resource and run metadata on the test span" do
      original = OpenTelemetry.tracer_provider
      OpenTelemetry.tracer_provider = OpenTelemetry::Internal::ProxyTracerProvider.new
      exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) { exporter }
      allow(OpenTelemetry::Instrumentation.registry).to receive(:install_all)
      allow(Buildkite::TestCollector).to receive(:test_runner).and_return("rspec")
      allow(ENV).to receive(:[]).and_call_original
      allow(ENV).to receive(:[]).with("BUILDKITE_TAG").and_return(nil)
      allow(ENV).to receive(:[]).with("BUILDKITE_BUILD_ID").and_return("build-123")
      allow(ENV).to receive(:[]).with("BUILDKITE_BUILD_URL")
        .and_return("https://buildkite.example/acme/payments/builds/123")
      allow(ENV).to receive(:[]).with("BUILDKITE_AGENT_ID").and_return("agent-123")
      allow(ENV).to receive(:[]).with("BUILDKITE_STEP_ID").and_return("step-123")
      allow(ENV).to receive(:[]).with("BUILDKITE_TEST_ENGINE_SUITE_SLUG").and_return("payments")
      allow(ENV).to receive(:[]).with("BUILDKITE_ORGANIZATION_SLUG").and_return("acme")

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        run_env: {
          "CI" => "buildkite",
          "key" => "run-123",
          "url" => "https://buildkite.example/acme/payments/builds/123",
          "branch" => "main",
          "commit_sha" => "abc123",
          "number" => "123",
          "job_id" => "job-123",
          "message" => "Test resource boundaries",
          "collector" => "ruby-buildkite-test_collector",
          "version" => Buildkite::TestCollector::VERSION,
          "language_version" => "3.4.1",
          "location_prefix" => "services/payments",
        },
        tags: { "team" => "platform", :speed => :fast },
      )

      expect(described_class).to be_enabled
      expect(OpenTelemetry.tracer_provider).not_to equal(original)

      root = described_class.start_test_span(test: execution_test)
      described_class.with_test_span(root) do
        OpenTelemetry.tracer_provider.tracer("test-suite").in_span("child") { nil }
      end
      described_class.finish_test_span(root, test: execution_test)
      described_class.force_flush

      root = exporter.finished_spans.find { |span| span.name == "test.execution" }
      child = exporter.finished_spans.find { |span| span.name == "child" }
      resource = root.resource.attribute_enumerator.to_h
      expect(resource).to include(
        "service.name" => "payments",
        "service.namespace" => "acme",
        "cicd.pipeline.run.id" => "build-123",
        "cicd.pipeline.run.url.full" => "https://buildkite.example/acme/payments/builds/123",
        "cicd.worker.id" => "agent-123",
        "process.runtime.version" => "3.4.1",
        "vcs.ref.head.name" => "main",
        "vcs.ref.head.revision" => "abc123",
        "vcs.ref.type" => "branch",
      )
      expect(resource).not_to include(
        "service.instance.id",
        "buildkite.run_key",
        "buildkite.run_url",
        "buildkite.build_number",
        "buildkite.job_id",
        "buildkite.message",
        "buildkite.step_id",
        "buildkite.collector.name",
        "buildkite.collector.version",
        "buildkite.location_prefix",
        "buildkite.test.framework.name",
        "buildkite.test.framework.version",
        "buildkite.tag.team",
      )
      expect(root.attributes).to include(
        "buildkite.run_key" => "run-123",
        "buildkite.build_number" => "123",
        "buildkite.job_id" => "job-123",
        "buildkite.message" => "Test resource boundaries",
        "buildkite.step_id" => "step-123",
        "buildkite.collector.name" => "ruby-buildkite-test_collector",
        "buildkite.collector.version" => Buildkite::TestCollector::VERSION,
        "buildkite.location_prefix" => "services/payments",
        "buildkite.test.framework.name" => "rspec",
        "buildkite.tag.team" => "platform",
        "buildkite.tag.speed" => "fast",
      )
      expect(root.attributes).not_to include("buildkite.run_url")
      expect(child.resource.attribute_enumerator.to_h).to include(resource)
      expect(child.attributes).not_to include(
        "buildkite.run_key",
        "buildkite.job_id",
        "buildkite.location_prefix",
        "buildkite.test.framework.name",
        "buildkite.tag.team",
      )
    ensure
      described_class.shutdown
      OpenTelemetry.tracer_provider = original
    end

    it "leaves a suite-installed provider in place while test spans carry the run metadata" do
      original = OpenTelemetry.tracer_provider
      suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      OpenTelemetry.tracer_provider = suite_provider
      exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      allow(OpenTelemetry::Exporter::OTLP::Exporter).to receive(:new) { exporter }

      described_class.configure!(
        endpoint: "https://example.invalid/v1/traces",
        run_env: { "key" => "run-123" },
      )

      # The suite's provider is not replaced or reconfigured: the
      # test span comes from the collector's private provider, and the suite's
      # spans reach Buildkite through the forwarder attached to its provider.
      expect(OpenTelemetry.tracer_provider).to equal(suite_provider)

      span = described_class.start_test_span(test: execution_test)
      described_class.finish_test_span(span, test: execution_test)
      described_class.force_flush

      exported = exporter.finished_spans.fetch(0)
      expect(exported.attributes).to include("buildkite.run_key" => "run-123")
      expect(exported.resource.attribute_enumerator.to_h).not_to include("buildkite.run_key")
    ensure
      described_class.shutdown
      suite_provider&.shutdown
      OpenTelemetry.tracer_provider = original
    end
  end

  describe ".annotate" do
    it "adds an annotation event to the current span" do
      exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      provider = OpenTelemetry::SDK::Trace::TracerProvider.new
      provider.add_span_processor(
        OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
      )
      tracer = provider.tracer("annotate-test")
      described_class.instance_variable_set(:@tracer, tracer)

      tracer.in_span("test.execution") do
        described_class.annotate("something happened")
      end
      provider.force_flush

      event = exporter.finished_spans.fetch(0).events.fetch(0)
      expect(event.name).to eq("test.annotation")
      expect(event.attributes).to eq("buildkite.annotation" => "something happened")
    ensure
      described_class.instance_variable_set(:@tracer, nil)
      provider&.shutdown
    end

    it "does nothing when export is off or no span is recording" do
      expect { described_class.annotate("ignored") }.not_to raise_error

      described_class.instance_variable_set(:@tracer, double("tracer"))
      expect { described_class.annotate("also ignored") }.not_to raise_error
    ensure
      described_class.instance_variable_set(:@tracer, nil)
    end
  end

  it "records the test's failure as span status and exception events when the test offers them" do
    exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    provider.add_span_processor(
      OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter)
    )
    described_class.instance_variable_set(:@tracer, provider.tracer("exception-test"))

    test = double(
      "trace",
      otel_attributes: {},
      otel_result: "failed",
      otel_failure_reason: "kaboom",
      otel_exception_events: [
        { "exception.message" => "kaboom", "exception.stacktrace" => "example.rb:1" },
      ],
    )

    span = described_class.start_test_span(test: execution_test)
    described_class.finish_test_span(span, test: test)
    provider.force_flush

    finished = exporter.finished_spans.fetch(0)
    event = finished.events.find { |e| e.name == "exception" }
    expect(event.attributes).to include(
      "exception.message" => "kaboom",
      "exception.stacktrace" => "example.rb:1",
    )
    expect(finished.status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    expect(finished.status.description).to eq("kaboom")
  ensure
    described_class.instance_variable_set(:@tracer, nil)
    provider&.shutdown
  end
end
