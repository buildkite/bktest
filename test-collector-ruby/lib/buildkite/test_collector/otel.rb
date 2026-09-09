# frozen_string_literal: true

require "securerandom"
require "uri"

module Buildkite::TestCollector
  module OTel
    DEFAULT_ENDPOINT = "https://tests-otlp.buildkite.com/v1/traces"

    # Accepted by the Buildkite OTLP traces receiver for its run-key header.
    RUN_KEY_FORMAT = /\A[!-~]{1,255}\z/

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

    # Standard OTLP exporter header variables, most specific first.
    HEADER_ENVIRONMENT_VARIABLES = %w[OTEL_EXPORTER_OTLP_TRACES_HEADERS OTEL_EXPORTER_OTLP_HEADERS].freeze

    TRACER_NAME = "buildkite-test-collector"

    TEST_SPAN_NAME = "test.execution"
    TEST_SPAN_MAX_QUEUE_SIZE = 8_192
    TEST_SPAN_MAX_EXPORT_BATCH_SIZE = 240
    TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS = 1_000
    TEST_SPAN_ATTRIBUTE_LENGTH_LIMIT = 4_096
    TEST_SPAN_EVENT_ATTRIBUTE_LENGTH_LIMIT = 16_384
    TEST_SPAN_EVENT_COUNT_LIMIT = 100

    module ExceptionHandling
      FATAL_EXCEPTIONS = [SystemExit, SignalException, NoMemoryError].freeze

      def self.reraise_fatal(exception)
        raise exception if FATAL_EXCEPTIONS.any? { |fatal| exception.is_a?(fatal) }
      end
    end
    private_constant :ExceptionHandling

    # BatchSpanProcessor#export_batch only rescues StandardError; a non-fatal
    # Exception must fail the batch without terminating its worker.
    module ExporterGuard
      @mutex = Mutex.new
      @warned_exception_classes = {}

      def export(spans, timeout: nil)
        super
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        # Mirror the exporter's own accounting for failures it detects, so the
        # dropped-span report can name this cause too.
        @metrics_reporter&.add_to_counter("otel.otlp_exporter.failure", labels: { "reason" => e.class.to_s })
        ExporterGuard.warn_once(e)
        OpenTelemetry::SDK::Trace::Export::FAILURE
      end

      class << self
        def warn_once(exception)
          first_failure = @mutex.synchronize do
            @warned_exception_classes[exception.class] = true unless @warned_exception_classes.key?(exception.class)
          end
          return unless first_failure

          # WebMock's message includes the request body and Authorization header.
          warn "[buildkite-test_collector] Could not export OpenTelemetry spans: #{exception.class}. " \
            "Further #{exception.class} export failures will not be reported."
        end

        def reset
          @mutex.synchronize { @warned_exception_classes.clear }
        end
      end
    end
    private_constant :ExporterGuard

    require_relative "otel/test_span_metrics_reporter"
    require_relative "otel/span_filter"
    require_relative "otel/child_span_forwarder"

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

      # Whether the standard OTLP environment supplies request headers (which
      # may carry the credential, as bktec's relay does).
      def headers_from_environment?(endpoint: ENV["BUILDKITE_ANALYTICS_OTLP_ENDPOINT"] || DEFAULT_ENDPOINT)
        return false unless raw_otlp_headers_from_environment
        return true if standard_otlp_endpoint_matches?(endpoint)

        warn_ignored_otlp_headers
        false
      end

      def configure!(endpoint: DEFAULT_ENDPOINT, api_token: nil, run_env: {}, span_filter: nil, tags: {})
        run_key = run_env["key"]
        unless enabled? || valid_run_key?(run_key)
          warn_invalid_run_key(run_key, api_token: api_token)
          # false tells start_otel that the fallback warning is already emitted.
          return false
        end

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

        require "opentelemetry/sdk"
        require "opentelemetry/exporter/otlp"
        require "opentelemetry/trace/propagation/trace_context"

        exempt_from_vcr(endpoint)
        exempt_from_webmock(endpoint)

        @api_token = api_token
        @run_key = run_env["key"]
        # Passing collector headers to the exporter bypasses its environment
        # defaults, so merge the standard OTLP headers here instead.
        environment_headers = otlp_headers_from_environment(endpoint)
        @authorization_from_environment = environment_headers.keys.any? do |key|
          key.casecmp?("Authorization")
        end
        headers = request_headers(run_env, api_token, environment_headers)

        # Resources identify the entities that produced the telemetry. Details
        # about the Test Engine run and test framework describe each execution
        # instead, so keep them on the test span rather than every child span.
        resource = producer_resource(run_env)
        @run_attributes = run_attributes(run_env, tags)

        @test_span_provider = build_test_span_provider(endpoint, headers, resource)
        @tracer = @test_span_provider.tracer(TRACER_NAME, Buildkite::TestCollector::VERSION)
        configure_child_export(endpoint, headers, resource, span_filter: span_filter)
        register_shutdown_at_exit
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] OpenTelemetry span export disabled: #{e.class}: #{e.message}"
        shutdown
      end

      def start_test_span(test:)
        return unless enabled?

        # The SDK retains the earliest attributes at its configured limit.
        # These three are the minimum needed to synthesize an execution. The
        # span is the submission, so every test span must carry the via marker.
        attributes = { EXECUTION_VIA_ATTRIBUTE => "otlp" }
        run_key = (@run_attributes || {})["buildkite.run_key"]
        attributes["buildkite.run_key"] = run_key if run_key
        # Reserve the result's position before test code can consume the
        # SDK's attribute budget. finish_test_span replaces this value.
        attributes[RESULT_ATTRIBUTE] = "unset"
        test.otel_attributes.each do |key, value|
          next if value.nil? || attributes.key?(key) || key.start_with?(TAG_ATTRIBUTE_PREFIX)

          attributes[key] = value
        end

        @tracer.start_span(
          TEST_SPAN_NAME,
          with_parent: OpenTelemetry::Context.empty,
          attributes: attributes,
          links: job_span_links,
          kind: :internal,
        )
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        # The example still runs, but with no span it reaches neither upload
        # path, so report it as a missing result rather than a stray warning.
        @test_span_metrics_reporter&.record_start_failure(e)
        nil
      end

      def with_test_span(span)
        return yield unless span

        OpenTelemetry::Context.with_value(test_span_context_key, span.context.trace_id) do
          OpenTelemetry::Trace.with_span(span) { yield }
        end
      end

      # "Now" as the SDK would stamp it: the realtime clock, in seconds.
      # Not Time.now, which suites that freeze time (Timecop) fake out.
      def current_timestamp
        Rational(Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond), 1_000_000_000)
      end

      # Each step warns and moves on rather than raising, so the span always
      # finishes: a half-described execution beats a missing one.
      def finish_test_span(span, test:, end_timestamp: nil)
        return unless span

        record_result(span, test)
        describe_test(span, test)
        finish_span(span, end_timestamp)
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not finish OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      # Records a point-in-time annotation as an event on whichever span is
      # current, which during a test is the test's own trace. Safe to call
      # when export is off or nothing is recording: it just does nothing.
      def annotate(content)
        return unless enabled?

        span = OpenTelemetry::Trace.current_span
        return unless span.recording?

        span.add_event("test.annotation", attributes: { "buildkite.annotation" => content.to_s })
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not annotate OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      # Pushes any finished spans out now without stopping export. Used at the
      # end of a suite when the process (and maybe another suite run) lives on.
      def force_flush
        error = each_export_queue(PROCESSOR_TIMEOUT_SECONDS) do |queue, remaining|
          queue.force_flush(timeout: remaining)
        end
        if error
          warn "[buildkite-test_collector] Could not flush OpenTelemetry spans: #{error.class}: #{error.message}"
        end
        # Report what this suite run has dropped so far. The SDK's flush stops
        # at the first rejected batch and re-queues the rest, so a persistent
        # failure leaves a balance that drains, and is reported, at shutdown.
        @test_span_metrics_reporter&.warn_dropped_total
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not flush OpenTelemetry spans: #{e.class}: #{e.message}"
      end

      def shutdown
        forwarder_error = deactivate_child_span_forwarder(@child_span_forwarder)
        export_error = each_export_queue(PROCESSOR_TIMEOUT_SECONDS) do |queue, remaining|
          queue.shutdown(timeout: remaining)
        end
        error = forwarder_error || export_error
        if error
          warn "[buildkite-test_collector] Could not shut down OpenTelemetry span export: #{error.class}: #{error.message}"
        end
        @test_span_metrics_reporter&.warn_dropped_total
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not shut down OpenTelemetry span export: #{e.class}: #{e.message}"
      ensure
        @test_span_provider = nil
        @child_span_processor = nil
        @child_span_forwarder = nil
        @exporters = nil
        ExporterGuard.reset
        @test_span_metrics_reporter = nil
        @api_token = nil
        @authorization_from_environment = nil
        @run_attributes = nil
        @run_key = nil
        @tracer = nil
      end

      private

      def valid_run_key?(run_key)
        run_key.is_a?(String) && run_key.valid_encoding? && run_key.ascii_only? && RUN_KEY_FORMAT.match?(run_key)
      end

      def warn_invalid_run_key(run_key, api_token:)
        fallback = if api_token
          "The collector is falling back to the JSON upload."
        else
          "OpenTelemetry is disabled; results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set."
        end
        warn "[buildkite-test_collector] Test results would be missing in OpenTelemetry mode because run key " \
          "#{run_key.inspect} is invalid. Fix BUILDKITE_ANALYTICS_KEY (or the CI variable used to generate it); " \
          "it must be 1-255 printable ASCII characters without spaces. #{fallback}"
      end

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

      def build_test_span_provider(endpoint, headers, resource)
        max_queue_size = span_processor_config_value("BUILDKITE_TEST_ENGINE_OTEL_TEST_SPAN_QUEUE_SIZE", default: TEST_SPAN_MAX_QUEUE_SIZE)
        max_export_batch_size = span_processor_config_value("BUILDKITE_TEST_ENGINE_OTEL_TEST_SPAN_BATCH_SIZE", default: TEST_SPAN_MAX_EXPORT_BATCH_SIZE)
        if max_export_batch_size > max_queue_size
          warn "[buildkite-test_collector] BUILDKITE_TEST_ENGINE_OTEL_TEST_SPAN_BATCH_SIZE must be <= " \
            "BUILDKITE_TEST_ENGINE_OTEL_TEST_SPAN_QUEUE_SIZE; using defaults " \
            "(batch #{TEST_SPAN_MAX_EXPORT_BATCH_SIZE}, queue #{TEST_SPAN_MAX_QUEUE_SIZE})"
          max_queue_size = TEST_SPAN_MAX_QUEUE_SIZE
          max_export_batch_size = TEST_SPAN_MAX_EXPORT_BATCH_SIZE
        end

        @test_span_metrics_reporter = TestSpanMetricsReporter.new
        test_span_processor = batch_processor(
          endpoint,
          headers,
          max_queue_size: max_queue_size,
          max_export_batch_size: max_export_batch_size,
          schedule_delay: TEST_SPAN_SCHEDULE_DELAY_MILLISECONDS,
          start_thread_on_boot: true,
          metrics_reporter: @test_span_metrics_reporter,
        )
        test_span_provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
          sampler: OpenTelemetry::SDK::Trace::Samplers::ALWAYS_ON,
          id_generator: SecureRandomIdGenerator,
          resource: resource,
          # Lengths are characters, not bytes; explicit limits isolate test spans
          # from the customer's OTEL_SPAN_* environment settings.
          span_limits: OpenTelemetry::SDK::Trace::SpanLimits.new(
            attribute_length_limit: TEST_SPAN_ATTRIBUTE_LENGTH_LIMIT,
            event_attribute_length_limit: TEST_SPAN_EVENT_ATTRIBUTE_LENGTH_LIMIT,
            event_count_limit: TEST_SPAN_EVENT_COUNT_LIMIT,
          ),
        )
        test_span_provider.add_span_processor(test_span_processor)
        test_span_provider
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        stop_processor(test_span_processor)
        raise
      end

      def span_processor_config_value(name, default:)
        value = ENV[name]
        return default if value.nil?
        return value.to_i if value.ascii_only? && value.match?(/\A[0-9]+\z/) && value.to_i.positive?

        warn "[buildkite-test_collector] #{name} must be a positive integer, using default #{default}"
        default
      end

      def batch_processor(endpoint, headers, metrics_reporter: nil, **processor_options)
        exporter = OpenTelemetry::Exporter::OTLP::Exporter.new(
          endpoint: endpoint,
          headers: headers,
          compression: "gzip",
          certificate_file: nil,
          client_certificate_file: nil,
          client_key_file: nil,
          ssl_verify_mode: OpenSSL::SSL::VERIFY_PEER,
          metrics_reporter: metrics_reporter,
        )
        exporter.singleton_class.prepend(ExporterGuard)
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
          headers = exporter.instance_variable_get(:@headers)
          next false unless headers.is_a?(Hash)

          headers["Authorization"] = value
          true
        end
        if refreshed < Array(@exporters).length
          warn "[buildkite-test_collector] Could not refresh the OTLP Authorization header; export continues with the previous token"
        end
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not refresh the OTLP Authorization header: #{e.class}: #{e.message}"
      end

      # VCR's additive ignore_request hook keeps collector traffic out of
      # cassettes without replacing the suite's network policy.
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
          rescue Exception => e # rubocop:disable Lint/RescueException
            ExceptionHandling.reraise_fatal(e)
            false
          end
        end
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not exempt the OTLP endpoint from VCR: #{e.class}: #{e.message}"
      end

      def exempt_from_webmock(endpoint)
        return unless defined?(::WebMock)

        config = ::WebMock::Config.instance
        # Array() returns an existing Array itself, and suites commonly pass a
        # frozen constant to disable_net_connect!, so build a new list.
        config.allow = Array(config.allow) + [URI(endpoint).host]
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not exempt the OTLP endpoint from WebMock: #{e.class}: #{e.message}"
      end

      # The collector-managed child provider carries the same producer resource
      # as the test span provider and installs every registered instrumentation;
      # the suite chooses instrumentation by which gems it requires. A
      # suite-owned provider keeps its own resource and instrumentation.
      def configure_child_export(endpoint, headers, resource, span_filter: nil)
        provider = OpenTelemetry.tracer_provider
        collector_managed = provider.is_a?(OpenTelemetry::Internal::ProxyTracerProvider)
        unless collector_managed || provider.respond_to?(:add_span_processor)
          raise "existing OpenTelemetry tracer provider does not support adding a span processor"
        end

        child_processor = batch_processor(endpoint, headers)
        child_forwarder = ChildSpanForwarder.new(
          child_processor,
          context_key: test_span_context_key,
          span_filter: span_filter,
        )

        if collector_managed
          OpenTelemetry::SDK.configure do |config|
            config.resource = resource
            config.id_generator = SecureRandomIdGenerator
            config.add_span_processor(child_forwarder)
            config.use_all
          end

          if OpenTelemetry.tracer_provider.is_a?(OpenTelemetry::Internal::ProxyTracerProvider)
            raise "OpenTelemetry SDK did not install a tracer provider"
          end
        else
          provider.add_span_processor(child_forwarder)
        end

        @child_span_processor = child_processor
        @child_span_forwarder = child_forwarder
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        deactivate_child_span_forwarder(child_forwarder)
        stop_processor(child_processor)
        warn "[buildkite-test_collector] OpenTelemetry child span export disabled: #{e.class}: #{e.message}; test.execution export remains enabled"
      end

      def test_span_context_key
        @test_span_context_key ||= OpenTelemetry::Context.create_key("buildkite.test.execution")
      end

      # User tags travel under the buildkite.tag. prefix, which the server
      # strips and turns into upload-level tags.
      def tag_attributes(tags)
        (tags || {}).to_h { |key, value| ["#{TAG_ATTRIBUTE_PREFIX}#{key}", value.to_s] }
      end

      # A resource identifies the entities that produced every span from the
      # provider: the suite, CI pipeline run and worker, and checked-out VCS ref.
      def producer_resource(run_env)
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
          attributes["vcs.ref.type"] = ENV["BUILDKITE_TAG"].to_s.empty? ? "branch" : "tag"
        end

        OpenTelemetry::SDK::Resources::Resource.default.merge(
          OpenTelemetry::SDK::Resources::Resource.create(
            attributes.compact
          )
        )
      end

      # These fields describe each test execution rather than the provider that
      # emitted its child spans. Configure-level tags apply to every test span;
      # tag_execution adds the per-test tags later when the result is finalized.
      def run_attributes(run_env, tags)
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

        attributes.compact.merge(tag_attributes(tags))
      end

      # Use provider-native IDs for correlation with other CI telemetry. The
      # Test Engine run key is a separate identity and stays on the test span.
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

      # Yields each export queue, test spans first, with what is left of one shared
      # budget so an unreachable endpoint cannot block the suite twice over.
      # Returns the first error rather than raising so every queue gets a turn.
      def each_export_queue(timeout)
        deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
        error = nil

        [@test_span_provider, @child_span_processor].compact.each do |queue|
          remaining = [deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC), 0].max
          begin
            yield queue, remaining
          rescue Exception => e # rubocop:disable Lint/RescueException
            ExceptionHandling.reraise_fatal(e)
            error ||= e
          end
        end

        error
      end

      def deactivate_child_span_forwarder(forwarder)
        forwarder&.shutdown
        nil
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        e
      end

      def stop_processor(processor)
        processor&.shutdown(timeout: 0)
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        nil
      end

      # How the test went: the result replaces the placeholder reserved at
      # start, and a failure also becomes the span status and exception events.
      def record_result(span, test)
        result = test.otel_result
        status = RESULT_STATUSES[result]
        span.set_attribute(RESULT_ATTRIBUTE, status) if status
        return unless result == "failed"

        # The failure summary rides as the span status description, and
        # each individual failure as a semconv exception event - the
        # native OTel shapes, which the server maps back to the
        # execution's failure_reason and failure_expanded.
        span.status = OpenTelemetry::Trace::Status.error(test.otel_failure_reason.to_s)
        test.otel_exception_events.each do |attributes|
          span.add_event("exception", attributes: attributes)
        end
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not record the OpenTelemetry test result: #{e.class}: #{e.message}"
      end

      # What the test was, and the run it belongs to.
      def describe_test(span, test)
        test_span_attributes(test).each do |key, value|
          span.set_attribute(key, value)
        end
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not describe OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      # The Ruby SDK keeps the earliest attributes when a span reaches its
      # limit, so order them by how much an execution needs them: the test
      # itself, then run metadata, then tags. Per-test tags win over
      # configure-level ones with the same key.
      def test_span_attributes(test)
        test_attributes = test.otel_attributes.compact
        run_attributes = @run_attributes || {}
        tag = ->(key, _) { key.start_with?(TAG_ATTRIBUTE_PREFIX) }

        test_attributes.reject(&tag)
          .merge(run_attributes.reject(&tag))
          .merge(run_attributes.merge(test_attributes).select(&tag))
      end

      def finish_span(span, end_timestamp)
        # A backwards clock step while the test ran can put the captured
        # realtime end before the span's start; fall back to the SDK's own
        # monotonic timing rather than export an invalid span.
        end_timestamp = nil if end_timestamp && precedes_start?(span, end_timestamp)

        if end_timestamp
          span.finish(end_timestamp: end_timestamp)
        else
          span.finish
        end
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        warn "[buildkite-test_collector] Could not finish OpenTelemetry test span: #{e.class}: #{e.message}"
      end

      def precedes_start?(span, end_timestamp)
        (end_timestamp.to_r * 1_000_000_000).to_i < span.start_timestamp
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
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
      rescue Exception => e # rubocop:disable Lint/RescueException
        ExceptionHandling.reraise_fatal(e)
        []
      end

      def request_headers(run_env, api_token, environment_headers = otlp_headers_from_environment(DEFAULT_ENDPOINT))
        headers = { "Buildkite-Tests-Run-Key" => run_env["key"] }
        headers["Authorization"] = authorization_header(api_token) if api_token
        environment_headers.each do |key, value|
          # The receiver header must carry the same validated identity as the
          # test spans, even when a relay supplies other request headers.
          next if key.casecmp?("Buildkite-Tests-Run-Key")

          headers.delete_if { |existing, _| existing.casecmp?(key) }
          headers[key] = value
        end
        headers
      end

      def otlp_headers_from_environment(endpoint)
        raw = raw_otlp_headers_from_environment
        return {} unless raw
        unless standard_otlp_endpoint_matches?(endpoint)
          warn_ignored_otlp_headers
          return {}
        end

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

      def raw_otlp_headers_from_environment
        HEADER_ENVIRONMENT_VARIABLES.map { |name| ENV[name] }.find { |value| !value.to_s.empty? }
      end

      def standard_otlp_endpoint_matches?(endpoint)
        standard_endpoint = ENV["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]
        append_traces_path = false
        if standard_endpoint.to_s.empty?
          standard_endpoint = ENV["OTEL_EXPORTER_OTLP_ENDPOINT"]
          return false if standard_endpoint.to_s.empty?

          append_traces_path = true
        end

        standard_endpoint = normalized_otlp_endpoint(standard_endpoint, append_traces_path: append_traces_path)
        collector_endpoint = normalized_otlp_endpoint(endpoint)
        standard_endpoint && standard_endpoint == collector_endpoint
      rescue URI::InvalidURIError
        false
      end

      def normalized_otlp_endpoint(endpoint, append_traces_path: false)
        uri = URI(endpoint)
        return unless uri.scheme && uri.host

        path = uri.path.sub(%r{/+\z}, "")
        path = "#{path}/v1/traces" if append_traces_path
        [uri.scheme.downcase, uri.host.downcase, uri.port, path]
      end

      def warn_ignored_otlp_headers
        return if @warned_ignored_otlp_headers

        @warned_ignored_otlp_headers = true
        warn "[buildkite-test_collector] Standard OpenTelemetry exporter headers are ignored for the Buildkite endpoint; " \
          "to use them, set OTEL_EXPORTER_OTLP_TRACES_ENDPOINT to the collector's full endpoint, " \
          "or OTEL_EXPORTER_OTLP_ENDPOINT to its base URL without /v1/traces."
      end

      def authorization_header(api_token)
        "Token token=\"#{api_token}\""
      end
    end
  end
end
