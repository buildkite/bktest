# frozen_string_literal: true

require "securerandom"
require "uri"

module Buildkite::TestCollector
  module OTel
    DEFAULT_ENDPOINT = "https://tests-otlp.buildkite.com/v1/traces"

    EXECUTION_VIA_ATTRIBUTE = "buildkite.execution.via"
    RESULT_ATTRIBUTE = "test.case.result.status"
    TAG_ATTRIBUTE_PREFIX = "buildkite.tag."

    # OpenTelemetry has no standard value for skipped tests.
    RESULT_STATUSES = {
      "passed" => "pass",
      "failed" => "fail",
      "skipped" => "skipped",
    }.freeze

    PROCESSOR_TIMEOUT_SECONDS = 30

    TRACER_NAME = "buildkite-test-collector"

    ROOT_SPAN_NAME = "test.execution"
    ROOT_MAX_QUEUE_SIZE = 8_192
    ROOT_MAX_EXPORT_BATCH_SIZE = 512
    ROOT_SCHEDULE_DELAY_MILLISECONDS = 1_000

    require_relative "otel/root_span_metrics_reporter"
    require_relative "otel/execution_child_forwarder"

    # Avoid duplicate IDs when test suites seed Ruby's global PRNG.
    module SecureRandomIdGenerator
      module_function

      def generate_trace_id
        generate(16)
      end

      def generate_span_id
        generate(8)
      end

      def generate(length)
        invalid_id = "\0" * length
        loop do
          id = SecureRandom.random_bytes(length)
          return id unless id == invalid_id
        end
      end
      private_class_method :generate
    end
    private_constant :SecureRandomIdGenerator

    class << self
      def enabled?
        !@tracer.nil?
      end

      def configure!(endpoint: DEFAULT_ENDPOINT, api_token: nil, run_env: {}, instrumentations: nil, execution_tags: {})
        if enabled?
          # One process serves one run: the exporters and providers live for
          # the whole process, so run identity is fixed at first configure.
          # Only credentials may change within that lifetime - a warm worker
          # re-running a suite can bring a fresh (e.g. expiring OIDC) token
          # that the exporters' snapshotted Authorization headers would
          # otherwise never learn about. A different run key means a new run,
          # which needs a new process; warn rather than misattribute silently.
          warn_run_mismatch(run_env)
          refresh_authorization(api_token)
          return
        end

        # Non-empty selections are reserved for future :all and preset support.
        # Raising fails open by design: the rescue below reports the reserved
        # value and disables export rather than crashing the suite.
        unless instrumentations.nil? || instrumentations == []
          raise ArgumentError, "otel_instrumentations must be omitted or []"
        end

        require "opentelemetry/sdk"
        require "opentelemetry/exporter/otlp"
        require "opentelemetry/trace/propagation/trace_context"

        exempt_from_vcr(endpoint)

        @api_token = api_token
        @run_key = run_env["key"]
        # Passing collector headers to the exporter bypasses its environment
        # defaults, so merge the standard OTLP headers here instead.
        environment_headers = otlp_headers_from_environment
        @authorization_from_environment = environment_headers.keys.any? do |key|
          key.casecmp?("Authorization")
        end
        headers = request_headers(run_env, api_token, environment_headers)

        # Resources identify the entities that produced the telemetry. Details
        # about the Test Engine run and test framework describe each execution
        # instead, so keep them on the test root rather than every child span.
        resource = provider_resource(run_env)
        @execution_attributes = execution_attributes(run_env, execution_tags)

        @execution_provider = build_execution_provider(endpoint, headers, resource)
        @tracer = @execution_provider.tracer(TRACER_NAME, Buildkite::TestCollector::VERSION)
        configure_child_export(endpoint, headers, instrumentations, resource: resource)
        register_shutdown_at_exit
      rescue LoadError, StandardError => e
        warn "[buildkite-test_collector] OpenTelemetry span export disabled: #{e.class}: #{e.message}"
        shutdown
      end

      def start_test_span(test: nil)
        return unless enabled?

        attributes = {}
        if test
          test_attributes = test.otel_attributes
          # The SDK retains the earliest attributes at its configured limit.
          # These three are the minimum needed to synthesize an execution.
          via = test_attributes[EXECUTION_VIA_ATTRIBUTE]
          attributes[EXECUTION_VIA_ATTRIBUTE] = via if via
          run_key = (@execution_attributes || {})["buildkite.run_key"]
          attributes["buildkite.run_key"] = run_key if run_key
          # Reserve the result's position before test code can consume the
          # SDK's attribute budget. finish_test_span replaces this value.
          attributes[RESULT_ATTRIBUTE] = "unset"
          test_attributes.each do |key, value|
            next if value.nil? || attributes.key?(key) || key.start_with?(TAG_ATTRIBUTE_PREFIX)

            attributes[key] = value
          end
        end

        @tracer.start_span(
          ROOT_SPAN_NAME,
          with_parent: OpenTelemetry::Context.empty,
          attributes: attributes,
          links: job_span_links,
          kind: :internal,
        )
      rescue StandardError => e
        warn "[buildkite-test_collector] Could not start OpenTelemetry test span: #{e.class}: #{e.message}"
        nil
      end

      def with_test_span(span)
        return yield unless span

        OpenTelemetry::Context.with_value(execution_context_key, span.context.trace_id) do
          OpenTelemetry::Trace.with_span(span) { yield }
        end
      end

      # "Now" as the SDK would stamp it: the realtime clock, in seconds.
      # Not Time.now, which suites that freeze time (Timecop) fake out.
      def current_timestamp
        Rational(Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond), 1_000_000_000)
      end

      def finish_test_span(span, test: nil, end_timestamp: nil)
        return unless span

        test_attributes = {}
        begin
          if test
            result = test.otel_result
            status = RESULT_STATUSES[result]
            span.set_attribute(RESULT_ATTRIBUTE, status) if status

            # The Ruby SDK keeps the earliest attributes when a span reaches
            # its limit, so record the test itself before run metadata or tags.
            test_attributes = test.otel_attributes.reject { |_, value| value.nil? }
            test_attributes.each do |key, value|
              span.set_attribute(key, value) unless key.start_with?(TAG_ATTRIBUTE_PREFIX)
            end

            if result == "failed"
              # The failure summary rides as the span status description, and
              # each individual failure as a semconv exception event - the
              # native OTel shapes, which the server maps back to the
              # execution's failure_reason and failure_expanded.
              reason = test.respond_to?(:otel_failure_reason) ? test.otel_failure_reason : nil
              span.status = OpenTelemetry::Trace::Status.error(reason.to_s)

              if test.respond_to?(:otel_exception_events)
                test.otel_exception_events.each do |attributes|
                  span.add_event("exception", attributes: attributes)
                end
              end
            end
          end
        rescue StandardError => e
          warn "[buildkite-test_collector] Could not describe OpenTelemetry test span: #{e.class}: #{e.message}"
        ensure
          begin
            execution_attributes = @execution_attributes || {}
            execution_attributes.each do |key, value|
              span.set_attribute(key, value) unless key.start_with?(TAG_ATTRIBUTE_PREFIX)
            end
            execution_attributes.merge(test_attributes).each do |key, value|
              span.set_attribute(key, value) if key.start_with?(TAG_ATTRIBUTE_PREFIX)
            end
          rescue StandardError => e
            warn "[buildkite-test_collector] Could not add OpenTelemetry run metadata: #{e.class}: #{e.message}"
          ensure
            finish_span(span, end_timestamp)
          end
        end
      end

      # Records a point-in-time annotation as an event on whichever span is
      # current, which during a test is the test's own trace. Safe to call
      # when export is off or nothing is recording: it just does nothing.
      def annotate(content)
        return unless enabled?

        span = OpenTelemetry::Trace.current_span
        return unless span.recording?

        span.add_event("test.annotation", attributes: { "buildkite.annotation" => content.to_s })
      rescue StandardError => e
        warn "[buildkite-test_collector] Could not annotate OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      # Pushes any finished spans out now without stopping export. Used at the
      # end of a suite when the process (and maybe another suite run) lives on.
      # Both queues share one budget, roots first, like shutdown_exports, so
      # an unreachable endpoint cannot block the suite twice over.
      def force_flush
        deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + PROCESSOR_TIMEOUT_SECONDS
        error = nil

        [@execution_provider, @execution_child_processor].compact.each do |component|
          remaining = [deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC), 0].max
          begin
            component.force_flush(timeout: remaining)
          rescue StandardError => e
            error ||= e
          end
        end

        if error
          warn "[buildkite-test_collector] Could not flush OpenTelemetry spans: #{error.class}: #{error.message}"
        end
      end

      def shutdown
        forwarder_error = deactivate_child_forwarder(@execution_child_forwarder)
        export_error = shutdown_exports(PROCESSOR_TIMEOUT_SECONDS)
        error = forwarder_error || export_error
        if error
          warn "[buildkite-test_collector] Could not shut down OpenTelemetry span export: #{error.class}: #{error.message}"
        end
      ensure
        @execution_provider = nil
        @execution_child_processor = nil
        @execution_child_forwarder = nil
        @exporters = nil
        @api_token = nil
        @authorization_from_environment = nil
        @execution_attributes = nil
        @run_key = nil
        @tracer = nil
      end

      private

      # Suite hooks only flush, because a suite's before/after(:suite) can run
      # more than once in a single process (warm test pools re-run suites).
      # The process-lifetime shutdown lives here instead, registered once on
      # first successful configure. The flag survives shutdown so an explicit
      # mid-process shutdown followed by a reconfigure cannot stack handlers;
      # shutdown is a safe no-op when nothing is configured.
      def register_shutdown_at_exit
        return if @shutdown_at_exit_registered

        @shutdown_at_exit_registered = true
        at_exit { Buildkite::TestCollector::OTel.shutdown }
      end

      def build_execution_provider(endpoint, headers, resource)
        execution_processor = batch_processor(
          endpoint,
          headers,
          max_queue_size: ROOT_MAX_QUEUE_SIZE,
          max_export_batch_size: ROOT_MAX_EXPORT_BATCH_SIZE,
          schedule_delay: ROOT_SCHEDULE_DELAY_MILLISECONDS,
          metrics_reporter: RootSpanMetricsReporter.new,
        )
        execution_provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
          sampler: OpenTelemetry::SDK::Trace::Samplers::ALWAYS_ON,
          id_generator: SecureRandomIdGenerator,
          resource: resource,
        )
        execution_provider.add_span_processor(execution_processor)
        execution_provider
      rescue StandardError
        stop_processor(execution_processor)
        raise
      end

      # One metrics reporter serves both the exporter and its processor, so it
      # can pair a dropped batch with the HTTP failure that caused it.
      def batch_processor(endpoint, headers, metrics_reporter: nil, **processor_options)
        exporter = OpenTelemetry::Exporter::OTLP::Exporter.new(
          endpoint: endpoint,
          headers: headers,
          metrics_reporter: metrics_reporter,
        )
        # Retained so refresh_authorization can reach the headers each
        # exporter snapshotted at construction.
        (@exporters ||= []) << exporter
        OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(
          exporter,
          metrics_reporter: metrics_reporter,
          **processor_options,
        )
      end

      # Run identity (run key, resource, Run-Key header) is fixed for the
      # process; reconfiguring with a different run key cannot take effect,
      # so make the misattribution visible instead of silent.
      def warn_run_mismatch(run_env)
        key = run_env["key"]
        return if key.nil? || key == @run_key

        warn "[buildkite-test_collector] OpenTelemetry span export is already configured for run #{@run_key.inspect} " \
          "and cannot switch to run #{key.inspect}: results will still be attributed to the earlier run. " \
          "Reporting a new run requires a new process."
      end

      # The OTLP exporter copies its headers at construction and offers no way
      # to change them, so a token refreshed between suite runs would never
      # reach the long-lived exporters and every later batch would carry the
      # expired token. Updating the snapshot in place reaches into the
      # exporter's internals for want of a public API; if those internals
      # change, the fallback is a warning and the previous token.
      def refresh_authorization(api_token)
        return if api_token.nil? || api_token == @api_token

        @api_token = api_token
        # Standard OTLP configuration remains authoritative across warm-worker
        # reconfiguration, even when the collector receives a refreshed token.
        return if @authorization_from_environment

        value = authorization_header(api_token)
        refreshed = Array(@exporters).count do |exporter|
          headers = exporter.instance_variable_defined?(:@headers) && exporter.instance_variable_get(:@headers)
          next false unless headers.is_a?(Hash)

          headers["Authorization"] = value
          true
        end
        if refreshed < Array(@exporters).length
          warn "[buildkite-test_collector] Could not refresh the OTLP Authorization header; export continues with the previous token"
        end
      rescue StandardError => e
        warn "[buildkite-test_collector] Could not refresh the OTLP Authorization header: #{e.class}: #{e.message}"
      end

      # Test suites that stub HTTP with VCR would otherwise intercept our
      # span export and fail the run (or record it into a cassette). VCR's
      # ignore_request hooks are additive, so this exempts exactly one
      # request shape - a POST to the configured OTLP endpoint - and leaves
      # the suite's network policy otherwise untouched. This runs from
      # RSpec's before(:suite), after the consumer's own VCR configuration
      # has loaded. WebMock used without VCR has no equivalent additive API,
      # so that case stays consumer-configured.
      def exempt_from_vcr(endpoint)
        return unless defined?(::VCR)

        target = URI(endpoint)
        ::VCR.configure do |vcr_config|
          vcr_config.ignore_request do |request|
            uri = URI(request.uri)
            request.method == :post &&
              uri.host == target.host &&
              uri.port == target.port &&
              uri.path == target.path
          rescue StandardError
            false
          end
        end
      rescue StandardError => e
        warn "[buildkite-test_collector] Could not exempt the OTLP endpoint from VCR: #{e.class}: #{e.message}"
      end

      # The collector-managed child provider carries the same producer resource
      # as the execution provider. A suite-owned provider keeps its own resource.
      def configure_child_export(endpoint, headers, instrumentations, resource: nil)
        provider = OpenTelemetry.tracer_provider
        collector_managed = provider.is_a?(OpenTelemetry::Internal::ProxyTracerProvider)
        unless collector_managed || provider.respond_to?(:add_span_processor)
          raise "existing OpenTelemetry tracer provider does not support adding a span processor"
        end

        child_processor = batch_processor(endpoint, headers)
        child_forwarder = ExecutionChildForwarder.new(
          child_processor,
          context_key: execution_context_key,
        )

        if collector_managed
          OpenTelemetry::SDK.configure do |config|
            config.resource = resource if resource
            config.id_generator = SecureRandomIdGenerator
            config.add_span_processor(child_forwarder)
            config.use_all if instrumentations.nil?
          end

          if OpenTelemetry.tracer_provider.is_a?(OpenTelemetry::Internal::ProxyTracerProvider)
            raise "OpenTelemetry SDK did not install a tracer provider"
          end
        else
          provider.add_span_processor(child_forwarder)
          unless instrumentations.nil?
            warn "[buildkite-test_collector] OpenTelemetry instrumentation selection ignored because the test suite already configured OpenTelemetry: #{instrumentations.inspect}"
          end
        end

        @execution_child_processor = child_processor
        @execution_child_forwarder = child_forwarder
      rescue StandardError => e
        deactivate_child_forwarder(child_forwarder)
        stop_processor(child_processor)
        warn "[buildkite-test_collector] OpenTelemetry child span export disabled: #{e.class}: #{e.message}; test.execution export remains enabled"
      end

      def execution_context_key
        @execution_context_key ||= OpenTelemetry::Context.create_key("buildkite.test.execution")
      end

      # User tags travel under the buildkite.tag. prefix, which the server
      # strips and turns into upload-level tags.
      def tag_attributes(tags)
        (tags || {})
          .map { |key, value| ["#{TAG_ATTRIBUTE_PREFIX}#{key}", value.to_s] }.to_h
      end

      # A resource identifies the entities that produced every span from the
      # provider: the suite, CI pipeline run and worker, and checked-out VCS ref.
      def provider_resource(run_env)
        pipeline_run_id, pipeline_run_url = ci_pipeline_run(run_env)
        worker_id = ENV["BUILDKITE_AGENT_ID"]
        worker_id = nil if worker_id.nil? || worker_id.strip.empty?

        attributes = {
          "service.name" => ENV["BUILDKITE_TEST_ENGINE_SUITE_SLUG"],
          "service.namespace" => ENV["BUILDKITE_ORGANIZATION_SLUG"],
          "cicd.pipeline.run.id" => pipeline_run_id,
          "cicd.pipeline.run.url.full" => pipeline_run_id && pipeline_run_url,
          "cicd.worker.id" => worker_id,
          "process.runtime.version" => run_env["language_version"],
          "vcs.ref.head.name" => run_env["branch"],
          "vcs.ref.head.revision" => run_env["commit_sha"],
        }
        if run_env["branch"]
          tag = ENV["BUILDKITE_TAG"]
          attributes["vcs.ref.type"] = tag.nil? || tag.empty? ? "branch" : "tag"
        end

        OpenTelemetry::SDK::Resources::Resource.default.merge(
          OpenTelemetry::SDK::Resources::Resource.create(
            attributes.reject { |_, value| value.nil? }
          )
        )
      end

      # These fields describe each test execution rather than the provider that
      # emitted its child spans. Configure-level tags apply to every test root;
      # tag_execution adds the per-test tags later when the result is finalized.
      def execution_attributes(run_env, execution_tags)
        _, pipeline_run_url = ci_pipeline_run(run_env)
        attributes = {
          "buildkite.run_key" => run_env["key"],
          "buildkite.build_number" => run_env["number"],
          "buildkite.job_id" => run_env["job_id"],
          "buildkite.message" => run_env["message"],
          "buildkite.step_id" => ENV["BUILDKITE_STEP_ID"],
          "buildkite.collector.name" => run_env["collector"],
          "buildkite.collector.version" => run_env["version"],
          "buildkite.location_prefix" => run_env["location_prefix"],
          "buildkite.test.framework.name" => Buildkite::TestCollector.test_runner,
        }
        if run_env["url"] && run_env["url"] != pipeline_run_url
          attributes["buildkite.run_url"] = run_env["url"]
        end
        if defined?(RSpec::Core::Version::STRING)
          attributes["buildkite.test.framework.version"] = RSpec::Core::Version::STRING
        end

        attributes.reject { |_, value| value.nil? }.merge(tag_attributes(execution_tags))
      end

      # Use provider-native IDs for correlation with other CI telemetry. The
      # Test Engine run key is a separate identity and stays on the test root.
      def ci_pipeline_run(run_env)
        case run_env["CI"]
        when "buildkite"
          [ENV["BUILDKITE_BUILD_ID"], ENV["BUILDKITE_BUILD_URL"]]
        when "github_actions"
          id = ENV["GITHUB_RUN_ID"]
          repository = ENV["GITHUB_REPOSITORY"]
          url = File.join("https://github.com", repository, "actions/runs", id) if repository && id
          [id, url]
        when "circleci"
          [ENV["CIRCLE_WORKFLOW_ID"], nil]
        when "codeship"
          [ENV["CI_BUILD_ID"], nil]
        else
          [nil, nil]
        end
      end

      def shutdown_exports(timeout)
        deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
        error = nil

        [@execution_provider, @execution_child_processor].compact.each do |component|
          remaining = [deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC), 0].max
          begin
            component.shutdown(timeout: remaining)
          rescue StandardError => e
            error ||= e
          end
        end

        error
      end

      def deactivate_child_forwarder(forwarder)
        forwarder&.shutdown
        nil
      rescue StandardError => e
        e
      end

      def stop_processor(processor)
        processor&.shutdown(timeout: 0)
      rescue StandardError
        nil
      end

      def finish_span(span, end_timestamp = nil)
        # A backwards clock step while the test ran can put the captured
        # realtime end before the span's start; fall back to the SDK's own
        # monotonic timing rather than export an invalid span.
        end_timestamp = nil if end_timestamp && precedes_start?(span, end_timestamp)

        if end_timestamp
          span.finish(end_timestamp: end_timestamp)
        else
          span.finish
        end
      rescue StandardError => e
        warn "[buildkite-test_collector] Could not finish OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      def precedes_start?(span, end_timestamp)
        return false unless span.respond_to?(:start_timestamp) && span.start_timestamp

        (end_timestamp.to_r * 1_000_000_000).to_i < span.start_timestamp
      rescue StandardError
        false
      end

      # Link to the Agent job while keeping each execution a trace root.
      def job_span_links
        carrier = {
          "traceparent" => ENV["TRACEPARENT"],
          "tracestate" => ENV["TRACESTATE"],
        }
        context = OpenTelemetry::Trace::Propagation::TraceContext
          .text_map_propagator
          .extract(carrier, context: OpenTelemetry::Context.empty)
        span_context = OpenTelemetry::Trace.current_span(context).context
        return [] unless span_context.valid?

        [OpenTelemetry::Trace::Link.new(span_context)]
      rescue StandardError
        []
      end

      def request_headers(run_env, api_token, environment_headers = otlp_headers_from_environment)
        headers = { "Buildkite-Tests-Run-Key" => run_env["key"] }
        headers["Authorization"] = authorization_header(api_token) if api_token
        environment_headers.each do |key, value|
          headers.delete_if { |existing, _| existing.casecmp?(key) }
          headers[key] = value
        end
        headers
      end

      def otlp_headers_from_environment
        raw = ENV["OTEL_EXPORTER_OTLP_TRACES_HEADERS"]
        raw = ENV["OTEL_EXPORTER_OTLP_HEADERS"] if raw.nil? || raw.empty?
        return {} if raw.nil? || raw.empty?

        entries = raw.split(",")
        raise ArgumentError, "invalid OTLP exporter headers" if entries.empty?

        entries.each_with_object({}) do |entry, headers|
          key, value = entry.split("=", 2).map { |part| URI.decode_uri_component(part) }
          key = key.to_s.strip
          value = value.to_s.strip
          raise ArgumentError, "invalid OTLP exporter headers" if key.empty? || value.empty?

          headers.delete_if { |existing, _| existing.casecmp?(key) }
          headers[key] = value
        end
      end

      def authorization_header(api_token)
        "Token token=\"#{api_token}\""
      end
    end
  end
end
