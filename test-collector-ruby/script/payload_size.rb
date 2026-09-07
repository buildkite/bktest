#!/usr/bin/env ruby
# frozen_string_literal: true

# Measure synthetic RSpec OTLP requests without exporting anything (Ruby 3.3+).
# Run from test-collector-ruby after bundle install:
#   bundle exec ruby script/payload_size.rb
#   bundle exec ruby script/payload_size.rb --spans 128 --backtrace-lines 170 --message-bytes 20480
# Defaults to the shipped batch size and five representative rows. Supplying
# either failure option selects one custom failing row (the other defaults to
# 100 lines / 10 KiB). Message bytes are exact ASCII input bytes, before applying
# the RSpec plugin's real limits. Backtraces vary line numbers and method names;
# diffs and test attributes vary per example rather than repeating one span.
# This estimates compressible, single-exception payloads, not worst-case sizes.
# Uses the exporter's private #encode API, so SDK upgrades may require changes.

require "bundler/setup"
require "opentelemetry/sdk"
require "opentelemetry/exporter/otlp"
require "optparse"
require "zlib"
require_relative "../lib/buildkite/test_collector"
require_relative "../lib/buildkite/test_collector/rspec_plugin/trace"

def span_data(i, failing:, backtrace_lines:, message_bytes:)
  attrs = {
    "buildkite.execution.via" => "otlp",
    "buildkite.run_key" => "83d96bfd-2388-4508-a8eb-070df6648da8",
    "test.case.result.status" => failing ? "fail" : "pass",
    "buildkite.test.scope" => "Some::Deeply::Nested::ServiceObject with a long context description ##{i}",
    "buildkite.test.name" => "does the thing when the other thing is configured correctly ##{i}",
    "test.case.name" => "Some::Deeply::Nested::ServiceObject with a long context description does the thing when the other thing is configured correctly ##{i}",
    "test.suite.name" => "Some::Deeply::Nested::ServiceObject with a long context description",
    "code.file.path" => "./spec/services/some/deeply/nested/service_object_spec_#{i % 50}.rb",
    "code.line.number" => 120 + i % 300,
    "buildkite.test.execution.external_id" => "019a0b3c-#{i.to_s.rjust(4, '0')}-7abc-8def-0123456789ab",
    "buildkite.build_number" => "123456",
    "buildkite.job_id" => "019a0b3c-1234-7abc-8def-0123456789ab",
    "buildkite.message" => "Merge pull request #1234 from org/branch: fix the thing",
    "buildkite.step_id" => "019a0b3c-5678-7abc-8def-0123456789ab",
    "buildkite.collector.name" => "ruby-buildkite-test_collector",
    "buildkite.collector.version" => Buildkite::TestCollector::VERSION,
    "buildkite.test.framework.name" => "rspec",
    "buildkite.test.framework.version" => "3.13.0",
    "buildkite.tag.worker" => "agent-#{i % 8}",
  }
  timestamp = 1_700_000_000_000_000_000
  events = []
  status = OpenTelemetry::Trace::Status.unset
  if failing
    diff = <<~DIFF
      expected: {"id"=>#{i}, "status"=>"active", "email"=>"user#{i}@example.com", "created_at"=>"2026-09-01T10:00:00Z"}
           got: {"id"=>#{i}, "status"=>"pending", "email"=>"user#{i}@example.com", "created_at"=>nil}

      (compared using ==)

      Diff:
      @@ -1,4 +1,4 @@
      -"status" => "active",
      +"status" => "pending",
    DIFF
    message = (diff * (message_bytes.fdiv(diff.bytesize).ceil)).byteslice(0, message_bytes)
    backtrace = Array.new(backtrace_lines) do |n|
      "./app/services/some/deeply/nested/service_object.rb:#{n * 7 + 12}:in 'Some::Deeply::Nested::ServiceObject#call_#{n}'"
    end
    trace = Buildkite::TestCollector::RSpecPlugin::Trace.new(
      nil,
      history: {},
      failure_reason: "Failure/Error: expected x got y ##{i}",
      failure_expanded: [{ expanded: [message], backtrace: backtrace }],
    )
    events = trace.otel_exception_events.map do |attributes|
      OpenTelemetry::SDK::Trace::Event.new("exception", attributes, timestamp)
    end
    status = OpenTelemetry::Trace::Status.error(trace.otel_failure_reason)
  end
  resource = OpenTelemetry::SDK::Resources::Resource.create(
    "service.name" => "my-suite",
    "service.namespace" => "my-org",
    "cicd.pipeline.run.id" => "019a0b3c-9999-7abc-8def-0123456789ab",
    "cicd.worker.id" => "019a0b3c-8888-7abc-8def-0123456789ab",
    "process.runtime.version" => RUBY_VERSION,
    "vcs.ref.head.name" => "main",
    "vcs.ref.head.revision" => "0123456789abcdef0123456789abcdef01234567",
  )
  OpenTelemetry::SDK::Trace::SpanData.new(
    "test.execution", :internal, status, OpenTelemetry::Trace::INVALID_SPAN_ID,
    attrs.size, events.size, 0, timestamp, timestamp + 50_000_000,
    attrs, [], events, resource,
    OpenTelemetry::SDK::InstrumentationScope.new("buildkite-test-collector", Buildkite::TestCollector::VERSION),
    OpenTelemetry::Trace.generate_span_id, OpenTelemetry::Trace.generate_trace_id,
    OpenTelemetry::Trace::TraceFlags::SAMPLED, OpenTelemetry::Trace::Tracestate::DEFAULT,
  )
end

options = { spans: Buildkite::TestCollector::OTel::TEST_SPAN_MAX_EXPORT_BATCH_SIZE }
parser = OptionParser.new do |opts|
  opts.banner = "Usage: bundle exec ruby script/payload_size.rb [options]"
  opts.on("--spans N", Integer, "Spans per request (default: #{options[:spans]})") { |n| options[:spans] = n }
  opts.on("--backtrace-lines N", Integer, "Custom failing row: backtrace input lines") { |n| options[:lines] = n }
  opts.on("--message-bytes N", Integer, "Custom failing row: ASCII message input bytes") { |n| options[:bytes] = n }
  opts.on("-h", "--help", "Show usage") { puts opts; exit }
end
begin
  parser.parse!
  raise OptionParser::InvalidArgument, "unexpected arguments: #{ARGV.join(' ')}" unless ARGV.empty?
  raise OptionParser::InvalidArgument, "spans must be positive" unless options[:spans].positive?
  if options.fetch(:lines, 0).negative? || options.fetch(:bytes, 0).negative?
    raise OptionParser::InvalidArgument, "lines and bytes must be nonnegative"
  end
rescue OptionParser::ParseError => e
  abort "#{e.message}\n#{parser}"
end

rows = if options.key?(:lines) || options.key?(:bytes)
  [[true, options.fetch(:lines, 100), options.fetch(:bytes, 10 * 1_024)]]
else
  [[false, 0, 0], [true, 30, 1_024], [true, 60, 4 * 1_024], [true, 100, 10 * 1_024], [true, 170, 10 * 1_024]]
end
# Only #encode is called: no processor, export, or network request is started.
exporter = OpenTelemetry::Exporter::OTLP::Exporter.new(endpoint: "http://127.0.0.1:1/v1/traces")
puts "RSpec limits applied; limits: raw <= 8192 KiB, gzip <= 900 KiB"
rows.each do |failing, lines, bytes|
  spans = Array.new(options[:spans]) { |i| span_data(i, failing: failing, backtrace_lines: lines, message_bytes: bytes) }
  raw = exporter.send(:encode, spans)
  gzip = Zlib.gzip(raw)
  printf "%d spans failing=%-5s backtrace_lines=%3d message_bytes=%5d: raw=%7.1f KiB gzip=%6.1f KiB%s%s\n",
    options[:spans], failing, lines, bytes, raw.bytesize / 1024.0, gzip.bytesize / 1024.0,
    raw.bytesize > 8 * 1024 * 1024 ? " OVER decoded limit" : "",
    gzip.bytesize > 900 * 1024 ? " OVER gzip limit" : ""
end
