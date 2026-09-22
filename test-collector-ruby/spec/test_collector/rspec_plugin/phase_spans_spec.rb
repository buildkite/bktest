# frozen_string_literal: true

require "opentelemetry/sdk"
require "rspec/core/sandbox"
require "buildkite/test_collector/rspec_plugin/reporter"

# Stands in for auto-instrumentation: a span from the global provider. A
# module method because sandboxed hooks and examples run with their own self.
module PhaseSpanSpecInstrumentation
  def self.span(name, &block)
    OpenTelemetry.tracer_provider.tracer("app").in_span(name, &(block || proc { nil }))
  end
end

RSpec.describe Buildkite::TestCollector::RSpecPlugin::PhaseSpans do
  # Sandboxed so the collector's hooks don't disturb this suite. The block
  # configures the sandboxed RSpec before the example group is defined.
  def run_sandboxed_example(body: proc { nil }, &configure)
    RSpec::Core::Sandbox.sandboxed do |config|
      config.output_stream = StringIO.new
      load "buildkite/test_collector/library_hooks/rspec.rb"
      config.add_formatter Buildkite::TestCollector::RSpecPlugin::Reporter
      configure&.call(config)

      group = RSpec.describe("phase group") do
        it("does something", &body)
      end
      group.run(RSpec.configuration.reporter)
      group.examples.first
    end
  end

  around do |test|
    original_otel_enabled = Buildkite::TestCollector.otel_enabled
    original_provider = OpenTelemetry.tracer_provider
    Buildkite::TestCollector.otel_enabled = true

    @exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    @provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    @provider.add_span_processor(
      OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(@exporter)
    )
    Buildkite::TestCollector::OTel.instance_variable_set(
      :@tracer, @provider.tracer("phase-spans-test")
    )
    # Phase spans and instrumentation come from the global provider.
    OpenTelemetry.tracer_provider = @provider

    test.run
  ensure
    Buildkite::TestCollector.otel_enabled = original_otel_enabled
    Buildkite::TestCollector::OTel.instance_variable_set(:@tracer, nil)
    OpenTelemetry.tracer_provider = original_provider
    @provider&.shutdown
  end

  def finished_spans
    @provider.force_flush
    @exporter.finished_spans
  end

  def span_named(name)
    finished_spans.find { |span| span.name == name }
  end

  def phase_spans
    finished_spans.select { |span| span.name.start_with?("test.") && span.name != "test.execution" }
  end

  it "adds setup, body, and teardown spans as direct children of the test span, in order" do
    run_sandboxed_example do |config|
      config.before(:each) { nil }
      config.after(:each) { nil }
    end
    test_span = span_named("test.execution")

    expect(phase_spans.map(&:name)).to contain_exactly("test.setup", "test.body", "test.teardown")
    expect(phase_spans.map(&:parent_span_id)).to all(eq(test_span.span_id))
    expect(phase_spans.map(&:trace_id)).to all(eq(test_span.trace_id))
    expect(phase_spans.map(&:status).map(&:code)).to all(eq(OpenTelemetry::Trace::Status::UNSET))

    setup, body, teardown = %w[test.setup test.body test.teardown].map { |name| span_named(name) }
    expect(setup.end_timestamp).to be <= body.start_timestamp
    expect(body.end_timestamp).to be <= teardown.start_timestamp
    expect(test_span.start_timestamp).to be <= setup.start_timestamp
  end

  it "nests instrumentation under the phase it ran in, and around hooks under the test span" do
    run_sandboxed_example(body: proc { PhaseSpanSpecInstrumentation.span("in body") }) do |config|
      config.before(:each) { PhaseSpanSpecInstrumentation.span("in before") }
      config.after(:each) { PhaseSpanSpecInstrumentation.span("in after") }
      config.around(:each) do |example|
        PhaseSpanSpecInstrumentation.span("in around") { example.run }
      end
    end

    expect(span_named("in before").parent_span_id).to eq(span_named("test.setup").span_id)
    expect(span_named("in body").parent_span_id).to eq(span_named("test.body").span_id)
    expect(span_named("in after").parent_span_id).to eq(span_named("test.teardown").span_id)
    expect(span_named("in around").parent_span_id).to eq(span_named("test.execution").span_id)
    # The around hook's span is current when the phases start, but must not
    # capture them.
    expect(phase_spans.map(&:parent_span_id)).to all(eq(span_named("test.execution").span_id))
  end

  it "fails the setup span and skips the body span when a before hook raises" do
    example = run_sandboxed_example(body: proc { raise "body must not run" }) do |config|
      config.before(:each) { raise "before boom" }
    end
    setup = span_named("test.setup")

    expect(example.execution_result.status).to eq(:failed)
    expect(phase_spans.map(&:name)).to contain_exactly("test.setup", "test.teardown")
    expect(setup.status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    expect(setup.status.description).to eq("before boom")
    expect(setup.events.find { |event| event.name == "exception" }.attributes).to include(
      "exception.message" => "before boom"
    )
    expect(span_named("test.teardown").status.code).to eq(OpenTelemetry::Trace::Status::UNSET)
  end

  it "does not fail the setup span when a before hook skips the example" do
    example = run_sandboxed_example(body: proc { raise "body must not run" }) do |config|
      config.before(:each) { skip "not today" }
    end

    expect(example.execution_result.status).to eq(:pending)
    expect(phase_spans.map(&:name)).to contain_exactly("test.setup", "test.teardown")
    expect(span_named("test.setup").status.code).to eq(OpenTelemetry::Trace::Status::UNSET)
    expect(span_named("test.setup").events).to be_nil
  end

  it "fails only the body span when the example raises" do
    run_sandboxed_example(body: proc { raise "body boom" }) do |config|
      config.after(:each) { nil }
    end

    expect(span_named("test.setup").status.code).to eq(OpenTelemetry::Trace::Status::UNSET)
    expect(span_named("test.body").status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    expect(span_named("test.body").status.description).to eq("body boom")
    expect(span_named("test.teardown").status.code).to eq(OpenTelemetry::Trace::Status::UNSET)
  end

  it "fails only the teardown span when an after hook raises" do
    example = run_sandboxed_example do |config|
      config.after(:each) { raise "after boom" }
    end

    expect(example.execution_result.status).to eq(:failed)
    expect(span_named("test.body").status.code).to eq(OpenTelemetry::Trace::Status::UNSET)
    expect(span_named("test.teardown").status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    expect(span_named("test.teardown").status.description).to eq("after boom")
  end

  # RSpec folds a second failure into a MultipleExceptionError wrapping the
  # first, and a third into that same object.
  it "attributes each failure to its own phase when the body and two after hooks all raise" do
    run_sandboxed_example(body: proc { raise "body boom" }) do |config|
      config.after(:each) { raise "first after boom" }
      config.after(:each) { raise "second after boom" }
    end

    expect(span_named("test.body").status.description).to eq("body boom")
    expect(span_named("test.teardown").status.code).to eq(OpenTelemetry::Trace::Status::ERROR)
    # After hooks run in reverse declaration order.
    expect(span_named("test.teardown").status.description).to eq("first after boom")
    exception_messages = span_named("test.teardown").events.map { |event| event.attributes["exception.message"] }
    expect(exception_messages).to eq(["first after boom"])
  end

  it "keeps annotations made during the example on the test span" do
    run_sandboxed_example(body: proc { Buildkite::TestCollector.annotate("checkpoint") })

    annotation = span_named("test.execution").events.find { |event| event.name == "test.annotation" }
    expect(annotation.attributes).to eq("buildkite.annotation" => "checkpoint")
    expect(span_named("test.body").events).to be_nil
  end

  it "leaves the context as it found it" do
    current_after = nil
    run_sandboxed_example do |config|
      config.around(:each) do |example|
        example.run
        current_after = OpenTelemetry::Trace.current_span
      end
    end

    expect(current_after.context.span_id).to eq(span_named("test.execution").span_id)
    expect(OpenTelemetry::Trace.current_span).to eq(OpenTelemetry::Trace::Span::INVALID)
  end

  it "adds no phase spans when OpenTelemetry is disabled" do
    Buildkite::TestCollector.otel_enabled = false
    Buildkite::TestCollector::OTel.instance_variable_set(:@tracer, nil)

    example = run_sandboxed_example(body: proc { PhaseSpanSpecInstrumentation.span("in body") }) do |config|
      config.before(:each) { nil }
    end

    expect(example.execution_result.status).to eq(:passed)
    expect(phase_spans).to be_empty
    expect(span_named("in body")).not_to be_nil
  end

  # The hooks wrap private rspec-core methods; a rename upstream would
  # silently turn the phases off.
  it "wraps the rspec-core methods it expects" do
    %i[run_before_example run_after_example].each do |name|
      method = RSpec::Core::Example.instance_method(name)
      expect(method.owner).to eq(described_class::ExampleHooks)
      expect(method.super_method.owner).to eq(RSpec::Core::Example)
    end
  end
end
