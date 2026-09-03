# frozen_string_literal: true

RSpec.describe Buildkite::TestCollector do
  # Perhaps there's a better way to make a stubbed ENV overlay that resets between tests.
  # We could probably use allow(ENV).to receive(...) although I find that more fragile.
  # Also, I hadn't seen spec/support/fake_env_helpers.rb when I wrote this :|
  ENV_REAL = ENV
  let(:env_overlay) { Hash.new { |_h, k| ENV_REAL[k] } }
  before { stub_const("ENV", env_overlay) }

  context "RSpec" do
    let(:hook) { :rspec }

    around do |test|
      original_otel_enabled = Buildkite::TestCollector.otel_enabled
      test.run
    ensure
      Buildkite::TestCollector.otel_enabled = original_otel_enabled
    end

    it "can configure api_token and url" do
      analytics = Buildkite::TestCollector
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"

      analytics.configure(hook: hook)

      expect(analytics.api_token).to eq "MyToken"
      expect(analytics.url).to eq "https://analytics-api.buildkite.com/v1/uploads"
    end

    it "can configure custom env" do
      analytics = Buildkite::TestCollector
      env = { test: "test value" }

      analytics.configure(hook: hook, env: env)

      expect(analytics.env).to match env
    end

    it "can configure (and unconfigure) trace_min_duration" do
      Buildkite::TestCollector.configure(hook: hook)
      expect(Buildkite::TestCollector.trace_min_duration).to eq(nil)

      env_overlay["BUILDKITE_ANALYTICS_TRACE_MIN_MS"] = "123"
      Buildkite::TestCollector.configure(hook: hook)
      expect(Buildkite::TestCollector.trace_min_duration).to eq(0.123)

      env_overlay.delete("BUILDKITE_ANALYTICS_TRACE_MIN_MS")
      Buildkite::TestCollector.configure(hook: hook)
      expect(Buildkite::TestCollector.trace_min_duration).to eq(nil)
    end

    it "leaves OpenTelemetry off unless it is opted into" do
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)

      Buildkite::TestCollector.configure(hook: hook)
      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector.otel_enabled?).to eq false
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)
    end

    it "stores OpenTelemetry options without setting it up during configuration" do
      run_env = { "key" => "run-key" }
      allow(Buildkite::TestCollector::CI).to receive(:env) { run_env }
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"

      Buildkite::TestCollector.configure(
        hook: hook,
        tracing_enabled: false,
        otel_enabled: true,
        otel_instrumentations: [],
        tags: { "team" => "platform" },
      )

      expect(Buildkite::TestCollector.otel_enabled?).to eq true
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)

      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector::OTel).to have_received(:configure!).with(
        endpoint: "https://tests-otlp.buildkite.com/v1/traces",
        api_token: "MyToken",
        run_env: run_env,
        instrumentations: [],
        tags: { "team" => "platform" },
      )
    end

    it "leaves OpenTelemetry off without a credential, like the JSON path" do
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = nil
      Buildkite::TestCollector::OTel::HEADER_ENVIRONMENT_VARIABLES.each { |name| env_overlay[name] = nil }

      expect {
        Buildkite::TestCollector.configure(hook: hook, otel_enabled: true)
        Buildkite::TestCollector.start_otel
      }.not_to output.to_stderr

      expect(Buildkite::TestCollector.otel_enabled?).to eq false
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)
    end

    it "keeps OpenTelemetry on when the OTLP environment supplies the headers" do
      allow(Buildkite::TestCollector::CI).to receive(:env) { { "key" => "run-key" } }
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = nil
      env_overlay["OTEL_EXPORTER_OTLP_TRACES_HEADERS"] = "Authorization=Bearer%20relay"

      Buildkite::TestCollector.configure(hook: hook, otel_enabled: true)
      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector.otel_enabled?).to eq true
      expect(Buildkite::TestCollector::OTel).to have_received(:configure!).with(hash_including(api_token: nil))
    end

    it "can override the endpoint for local development" do
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"
      env_overlay["BUILDKITE_ANALYTICS_OTLP_ENDPOINT"] = "http://tests-otlp.buildkite.localhost/v1/traces"
      allow(Buildkite::TestCollector::CI).to receive(:env) { { "key" => "run-key" } }
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }

      Buildkite::TestCollector.configure(hook: hook, otel_enabled: true)
      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector::OTel).to have_received(:configure!).with(
        hash_including(
          endpoint: "http://tests-otlp.buildkite.localhost/v1/traces",
        ),
      )
    end

    it "submits results only via OTLP when OpenTelemetry is enabled, with tags on test spans" do
      run_env = { "key" => "run-key" }
      allow(Buildkite::TestCollector::CI).to receive(:env) { run_env }
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { true }
      # Stubbed so another copy of the RSpec hooks is not installed here.
      allow(Buildkite::TestCollector).to receive(:hook_into)
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"
      env_overlay["BUILDKITE_AGENT_ID"] = "agent-123"

      Buildkite::TestCollector.configure(
        hook: hook,
        otel_enabled: true,
        tags: { "team" => "platform" },
      )

      expect(Buildkite::TestCollector.otel_enabled?).to eq true
      expect(Buildkite::TestCollector).to have_received(:hook_into).with(hook)
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)

      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector::OTel).to have_received(:configure!).with(
        endpoint: "https://tests-otlp.buildkite.com/v1/traces",
        api_token: "MyToken",
        run_env: run_env,
        instrumentations: nil,
        # The merged tags, so the automatic worker tag reaches OTLP too.
        tags: { "ci.worker.id" => "agent-123", "team" => "platform" },
      )
    end

    it "falls back to JSON when OpenTelemetry is enabled but could not be configured" do
      # configure! is stubbed to do nothing, so OTel stays disabled (as when
      # the gems are missing or Ruby is too old); the run must not upload nothing.
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)
      allow(Buildkite::TestCollector::OTel).to receive(:enabled?) { false }
      allow(Buildkite::TestCollector).to receive(:hook_into)
      allow(Buildkite::TestCollector::Network).to receive(:configure)
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"
      allow(Buildkite::TestCollector::Object).to receive(:configure)

      Buildkite::TestCollector.configure(hook: hook, otel_enabled: true)
      expect(Buildkite::TestCollector::Network).not_to have_received(:configure)

      expect {
        Buildkite::TestCollector.start_otel
      }.to output(/OpenTelemetry could not be configured .*; uploading results as JSON instead/).to_stderr

      expect(Buildkite::TestCollector.otel_enabled?).to eq false
      expect(Buildkite::TestCollector::Network).to have_received(:configure)
      expect(Buildkite::TestCollector::Object).to have_received(:configure)
    end

    it "routes annotations only to OpenTelemetry when it is enabled" do
      allow(Buildkite::TestCollector::OTel).to receive(:annotate)
      Buildkite::TestCollector.otel_enabled = true

      Buildkite::TestCollector.annotate("a thing happened")

      expect(Buildkite::TestCollector::OTel).to have_received(:annotate).with("a thing happened")
    end

    it "routes annotations only to the legacy trace when OpenTelemetry is off" do
      allow(Buildkite::TestCollector::OTel).to receive(:annotate)
      tracer = spy("tracer")
      allow(Buildkite::TestCollector::Uploader).to receive(:tracer) { tracer }

      Buildkite::TestCollector.annotate("a thing happened")

      expect(Buildkite::TestCollector::OTel).not_to have_received(:annotate)
      expect(tracer).to have_received(:enter).with("annotation", content: "a thing happened")
      expect(tracer).to have_received(:leave)
    end

    it "enables legacy tracing after OpenTelemetry without duplicating SQL subscriptions" do
      previous = Buildkite::TestCollector.instance_variable_get(:@active_support_tracing_enabled)
      Buildkite::TestCollector.instance_variable_set(:@active_support_tracing_enabled, nil)
      allow(Buildkite::TestCollector).to receive(:hook_into)
      allow(Buildkite::TestCollector::Network).to receive(:configure)
      allow(Buildkite::TestCollector::Object).to receive(:configure)
      allow(ActiveSupport::Notifications).to receive(:subscribe)
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"

      Buildkite::TestCollector.configure(hook: hook, otel_enabled: true)
      2.times { Buildkite::TestCollector.configure(hook: hook) }

      expect(Buildkite::TestCollector::Network).to have_received(:configure).twice
      expect(Buildkite::TestCollector::Object).to have_received(:configure).twice
      expect(ActiveSupport::Notifications).to have_received(:subscribe).once
    ensure
      Buildkite::TestCollector.instance_variable_set(:@active_support_tracing_enabled, previous)
    end
  end

  context "worker ID tag" do
    let(:hook) { :rspec }

    it "tags executions with ci.worker.id from BUILDKITE_AGENT_ID" do
      env_overlay["BUILDKITE_AGENT_ID"] = "agent-123"

      Buildkite::TestCollector.configure(hook: hook)

      expect(Buildkite::TestCollector.tags).to eq("ci.worker.id" => "agent-123")
    end

    it "omits the tag when BUILDKITE_AGENT_ID is unset" do
      env_overlay.delete("BUILDKITE_AGENT_ID")

      Buildkite::TestCollector.configure(hook: hook)

      expect(Buildkite::TestCollector.tags).to eq({})
    end

    it "omits the tag when BUILDKITE_AGENT_ID is empty" do
      env_overlay["BUILDKITE_AGENT_ID"] = ""

      Buildkite::TestCollector.configure(hook: hook)

      expect(Buildkite::TestCollector.tags).to eq({})
    end

    it "omits the tag when BUILDKITE_AGENT_ID is whitespace-only" do
      env_overlay["BUILDKITE_AGENT_ID"] = "   "

      Buildkite::TestCollector.configure(hook: hook)

      expect(Buildkite::TestCollector.tags).to eq({})
    end

    it "lets an explicit caller-supplied tag override the automatic one" do
      env_overlay["BUILDKITE_AGENT_ID"] = "agent-123"

      Buildkite::TestCollector.configure(hook: hook, tags: { "ci.worker.id" => "custom" })

      expect(Buildkite::TestCollector.tags).to eq("ci.worker.id" => "custom")
    end

    it "preserves other caller-supplied tags alongside the automatic one" do
      env_overlay["BUILDKITE_AGENT_ID"] = "agent-123"

      Buildkite::TestCollector.configure(hook: hook, tags: { "team" => "test-engine" })

      expect(Buildkite::TestCollector.tags).to eq(
        "ci.worker.id" => "agent-123",
        "team" => "test-engine",
      )
    end
  end

  context "Minitest" do
    let(:hook) { :minitest }

    it "can configure api_token and url" do
      analytics = Buildkite::TestCollector
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"

      analytics.configure(hook: hook)

      expect(analytics.api_token).to eq "MyToken"
      expect(analytics.url).to eq "https://analytics-api.buildkite.com/v1/uploads"
    end

    it "can configure custom env" do
      analytics = Buildkite::TestCollector
      env = { test: "test value" }

      analytics.configure(hook: hook, env: env)

      expect(analytics.env).to match env
    end

    it "warns and falls back to JSON instead of enabling the RSpec-only OpenTelemetry integration" do
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)

      expect {
        Buildkite::TestCollector.configure(
          hook: hook,
          otel_enabled: true,
        )
      }.to output(/otel_enabled is only supported with the rspec hook; the #{hook} hook will upload results as JSON/).to_stderr
      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector.otel_enabled?).to eq false
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)
    end
  end

  context "Cucumber" do
    let(:hook) { :cucumber }

    before do
      Cucumber::Runtime.new
    end

    it "can configure api_token and url" do
      analytics = Buildkite::TestCollector
      env_overlay["BUILDKITE_ANALYTICS_TOKEN"] = "MyToken"

      analytics.configure(hook: hook)

      expect(analytics.api_token).to eq "MyToken"
      expect(analytics.url).to eq "https://analytics-api.buildkite.com/v1/uploads"
    end

    it "can configure custom env" do
      analytics = Buildkite::TestCollector
      env = { test: "test value" }

      analytics.configure(hook: hook, env: env)

      expect(analytics.env).to match env
    end

    it "warns and falls back to JSON instead of enabling the RSpec-only OpenTelemetry integration" do
      allow(Buildkite::TestCollector::OTel).to receive(:configure!)

      expect {
        Buildkite::TestCollector.configure(
          hook: hook,
          otel_enabled: true,
        )
      }.to output(/otel_enabled is only supported with the rspec hook; the #{hook} hook will upload results as JSON/).to_stderr
      Buildkite::TestCollector.start_otel

      expect(Buildkite::TestCollector.otel_enabled?).to eq false
      expect(Buildkite::TestCollector::OTel).not_to have_received(:configure!)
    end
  end
end
