# frozen_string_literal: true

require "opentelemetry/sdk"
require "rspec/core/sandbox"

RSpec.describe "RSpec OpenTelemetry setup" do
  around do |test|
    original_otel_enabled = Buildkite::TestCollector.otel_enabled
    test.run
  ensure
    Buildkite::TestCollector.otel_enabled = original_otel_enabled
  end

  it "sets up OpenTelemetry after application libraries have loaded" do
    run_env = { "key" => "run-key" }
    library_loaded_when_configured = nil
    allow(Buildkite::TestCollector::CI).to receive(:env) { run_env }
    allow(ENV).to receive(:[]).and_call_original
    allow(ENV).to receive(:[]).with("BUILDKITE_ANALYTICS_TOKEN").and_return(nil)
    allow(ENV).to receive(:[]).with("BUILDKITE_ANALYTICS_OTLP_ENDPOINT").and_return(nil)
    allow(Buildkite::TestCollector::OTel).to receive(:configure!) do
      library_loaded_when_configured = defined?(LateLoadedApplicationLibrary)
    end
    allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }
    allow(Buildkite::TestCollector::OTel).to receive(:start_test_span)
    Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)

    RSpec::Core::Sandbox.sandboxed do |config|
      config.output_stream = StringIO.new
      load "buildkite/test_collector/library_hooks/rspec.rb"

      stub_const("LateLoadedApplicationLibrary", Module.new)
      group = RSpec.describe("Late-loaded application") { it("passes") { nil } }
      config.with_suite_hooks { group.run(RSpec.configuration.reporter) }
    end

    expect(Buildkite::TestCollector::OTel).to have_received(:configure!).with(
      endpoint: "https://tests-otlp.buildkite.com/v1/traces",
      api_token: nil,
      run_env: run_env,
      instrumentations: nil,
      execution_tags: {},
    )
    expect(library_loaded_when_configured).to eq("constant")
  end

  it "starts setup after the application configures its provider" do
    suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    active_provider = OpenTelemetry::Internal::ProxyTracerProvider.new
    provider_when_configured = nil
    allow(OpenTelemetry).to receive(:tracer_provider) { active_provider }
    allow(Buildkite::TestCollector::OTel).to receive(:configure!) do
      provider_when_configured = OpenTelemetry.tracer_provider
    end
    allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }
    allow(Buildkite::TestCollector::OTel).to receive(:start_test_span)
    Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)

    RSpec::Core::Sandbox.sandboxed do |config|
      config.output_stream = StringIO.new
      load "buildkite/test_collector/library_hooks/rspec.rb"

      active_provider = suite_provider
      group = RSpec.describe("Application-owned OpenTelemetry") { it("passes") { nil } }
      config.with_suite_hooks { group.run(RSpec.configuration.reporter) }
    end

    expect(provider_when_configured).to equal(suite_provider)
  ensure
    suite_provider&.shutdown
  end
end
