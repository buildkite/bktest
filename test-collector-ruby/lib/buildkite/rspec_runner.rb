# frozen_string_literal: true

require "rspec/core"
require "socket"
require "securerandom"
require "tempfile"
require_relative "test_collector"

module Buildkite
  # A work-source-agnostic client. bktec owns retries, muting and job status.
  class RSpecRunner
    class Error < StandardError; end

    DONE_REASONS = %w[plan_completed pool_consumed pool_errored terminating error].freeze
    SOCKET_ENV = "BUILDKITE_TEST_ENGINE_RUNNER_SOCKET"

    class Client
      attr_accessor :session_id, :read_timeout

      def initialize(path)
        @path = path
        @read_timeout = 35
      end

      def interrupt
        @interrupted = true
        @socket&.close
      end

      def request(method, path, payload, expected: 200)
        # Serialize once: after a lost response the host accepts only an exact replay.
        body = JSON.generate(payload)
        attempts = 0

        begin
          @socket = socket = UNIXSocket.new(@path)
          io = Net::BufferedIO.new(socket, read_timeout: read_timeout, write_timeout: 35)

          request = Net::HTTPGenericRequest.new(method, true, true, "/v1#{path}", {
            "Host" => "localhost", "Content-Type" => "application/json", "Connection" => "close",
          })
          # The initial handshake has no session; bktec assigns it in the response.
          request["X-Bktec-Session"] = session_id if session_id
          request.body = body
          request.exec(io, "1.1", "/v1#{path}")

          response = Net::HTTPResponse.read_new(io)
          response.reading_body(io, true) { response.body }
          raise Error, "#{method} #{path}: HTTP #{response.code}" unless response.code.to_i == expected

          value = JSON.parse(response.body)
          raise Error, "#{method} #{path}: expected a JSON object" unless value.is_a?(Hash)
          value
        rescue EOFError, IOError, SystemCallError, Net::ReadTimeout, Net::WriteTimeout
          socket&.close
          retry if !@interrupted && (attempts += 1) < 3
          raise
        ensure
          socket&.close
          @socket = nil
        end
      end
    end

    def self.run(args = ARGV)
      new.run(args)
    end

    def run(args)
      validate_options!(args)

      path = ENV.fetch(SOCKET_ENV) { raise Error, "#{SOCKET_ENV} is required" }
      raise Error, "#{SOCKET_ENV} must not be empty" if path.empty?

      @client = Client.new(path)
      install_signals

      boot_application
      return 1 if @stopping

      start_session

      until @stopping
        work = @client.request("POST", "/batches", {})
        break if @stopping

        case work["type"]
        when "batch"
          execute_batch(work.fetch("batch"))
        when "wait"
          delay = work["retry_after_ms"]
          raise Error, "invalid retry_after_ms" unless delay.is_a?(Integer) && delay >= 0

          # Short sleeps let signals stop the wait promptly.
          deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + delay / 1000.0
          sleep(0.05) until @stopping || Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
        when "done"
          raise Error, "unknown done reason" unless DONE_REASONS.include?(work["reason"])

          flush_collector
          return 0
        else
          raise Error, "unknown work response type"
        end
      end
      1
    rescue StandardError, LoadError, SyntaxError => e
      warn "[buildkite-rspec] #{e.class}: #{e.message}"
      1
    ensure
      begin
        flush_collector

        # A signal leaves the current batch unresolved rather than accepting partial work.
        if @client&.session_id && @stopping
          @client.request("DELETE", "/sessions/#{@client.session_id}", { reason: "signal_#{@stopping.downcase}" })
        end
      rescue StandardError => e
        warn "[buildkite-rspec] cleanup failed: #{e.message}"
      ensure
        @previous_signals&.each { |signal, handler| Signal.trap(signal, handler) }
      end
    end

    private

    def boot_application
      # Load support files and boot once, before asking for work.
      $LOAD_PATH.unshift(File.expand_path("spec")) unless $LOAD_PATH.include?(File.expand_path("spec"))
      @options.fetch(:libs, []).reverse_each { |dir| $LOAD_PATH.unshift(File.expand_path(dir)) }
      @options.fetch(:requires, []).each { |file| require file }
      require "rails_helper"

      # Preserve application collector settings, or install the default RSpec hooks.
      Buildkite::TestCollector.configure(hook: :rspec) unless Buildkite::TestCollector.test_runner
      validate_configuration!
    end

    def start_session
      session = @client.request("POST", "/sessions", {
        instance_id: SecureRandom.uuid,
        runner: { name: "buildkite-rspec", version: Buildkite::TestCollector::VERSION,
                  framework: "rspec", framework_version: RSpec::Core::Version::STRING,
                  language: "ruby", language_version: RUBY_VERSION, pid: Process.pid },
        capabilities: { selector_formats: %w[selector file example] },
      }, expected: 201)
      @client.session_id = nonempty_string!(session["session_id"], "session_id")

      # Allow a little longer than the host's maximum long-poll duration.
      max_wait = session.fetch("poll").fetch("max_wait_ms")
      raise Error, "invalid poll max_wait_ms" unless max_wait.is_a?(Integer) && max_wait >= 0
      @client.read_timeout = max_wait / 1000.0 + 5
    end

    def validate_options!(args)
      unless ENV.fetch("SPEC_OPTS", "").strip.empty?
        raise Error, "SPEC_OPTS is incompatible with the persistent runner"
      end

      @rspec_args = args.dup
      @options = RSpec::Core::ConfigurationOptions.new(@rspec_args).options

      unless @options.fetch(:files_or_directories_to_run, []).empty?
        raise Error, "caller selectors are not supported; bktec supplies batches"
      end

      if @options[:dry_run]
        raise Error, "--dry-run is incompatible with persistent batches: assigned tests must execute"
      end

      # Additional formatters are allowed, but cannot replace the private JSON
      # report appended by execute_batch. Work selection belongs to the host.
      safe = [:libs, :requires, :color, :color_mode, :order, :seed, :warnings,
              :backtrace, :custom_options_file, :formatters, :files_or_directories_to_run]
      invalid = @options.keys - safe
      raise Error, "incompatible persistent RSpec options: #{invalid.join(', ')}" unless invalid.empty?

      # CLI formatters replace options-file formatters in RSpec. Preserve the
      # resolved file formatters before appending our private CLI JSON formatter.
      unless RSpec::Core::Parser.parse(args).key?(:formatters)
        @options.fetch(:formatters, []).each do |formatter, output|
          @rspec_args.concat(["--format", formatter])
          @rspec_args.concat(["--out", output]) if output
        end
      end
    end

    def validate_configuration!
      config = RSpec.configuration
      unless Buildkite::TestCollector.test_runner == "rspec"
        raise Error, "the collector must use the rspec hook"
      end

      if defined?(RSpec::Retry)
        raise Error, "rspec-retry is incompatible with the persistent runner; bktec owns retries"
      end

      if config.dry_run || config.fail_fast || config.only_failures ||
          !config.inclusion_filter.empty? || !config.exclusion_filter.empty?
        raise Error, "RSpec filtering, dry_run, only_failures and fail_fast are incompatible with persistent batches"
      end
    end

    def install_signals
      @previous_signals = {}
      %w[INT TERM].each do |signal|
        @previous_signals[signal] = Signal.trap(signal) do
          @stopping = signal
          RSpec.world.wants_to_quit = true
          @client.interrupt
        end
      end
    end

    def nonempty_string!(value, name)
      raise Error, "invalid #{name}" unless value.is_a?(String) && !value.empty?
      value
    end

    def build_rspec_selectors(batch)
      tests = batch.fetch("tests")
      raise Error, "batch tests must be a nonempty array" unless tests.is_a?(Array) && !tests.empty?
      tests.map do |test|
        value = case test.fetch("format")
        when "selector" then test["value"]
        when "file" then test["path"]
        when "example" then test["identifier"].to_s.empty? ? test["path"] : test["identifier"]
        else raise Error, "unsupported selector format"
        end
        nonempty_string!(value, "test selector")
        raise Error, "test selector cannot be an option" if value.start_with?("-")
        value
      end
    end

    def execute_batch(batch)
      id = nonempty_string!(batch.fetch("id"), "batch id")
      # Batch IDs are opaque path segments, not URL paths.
      path = "/batches/#{URI.encode_www_form_component(id)}/results"
      args = build_rspec_selectors(batch)

      reset_batch

      Tempfile.create(["bktec-rspec-", ".json"]) do |report|
        # Runner.run installs its own INT trap. Use a subclass solely to retain
        # our cooperative signal handler, including on repeated INTs.
        runner = Class.new(RSpec::Core::Runner) do
          def self.trap_interrupt; end
        end

        begin
          runner.run(@rspec_args + args + ["--format", "json", "--out", report.path])
          return if @stopping # Partial reports must not acknowledge outstanding work.

          native_report = JSON.parse(File.read(report.path))
          raise Error, "RSpec report is not an object" unless native_report.is_a?(Hash)
        rescue StandardError, LoadError, SyntaxError => e
          return if @stopping

          @client.request("POST", path, {
            status: "errored", report_format: "rspec-json",
            error: { kind: "runner_error", message: "RSpec execution or JSON report failed: #{e.message}" },
          })
          raise Error, "RSpec did not produce a valid JSON report: #{e.message}"
        end

        # Flush executions before acknowledging the batch; bktec decides its outcome.
        flush_collector
        @client.request("POST", path, { status: "completed", report_format: "rspec-json", report: native_report })
      end
    end

    def reset_batch
      # Finish the previous collector session before clearing its execution records.
      flush_collector
      Buildkite::TestCollector.session = nil
      Buildkite::TestCollector::Uploader.traces.clear

      # Keep application configuration/hooks, but discard the previous batch's state.
      RSpec.clear_examples
      RSpec.world.wants_to_quit = !!@stopping
      RSpec.world.non_example_failure = false
      RSpec.world.rspec_is_quitting = false
    end

    def flush_collector
      Buildkite::TestCollector.session&.send_remaining_data
      Buildkite::TestCollector.session&.close
      Buildkite::TestCollector::OTel.force_flush
    end
  end
end
