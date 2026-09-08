#!/usr/bin/env ruby
# frozen_string_literal: true

# Measure synthetic RSpec OTLP requests without exporting anything (Ruby 3.3+).
# Run from test-collector-ruby after bundle install:
#   bundle exec ruby script/payload_size.rb
#   bundle exec ruby script/payload_size.rb --spans 128 --backtrace-lines 170 --message-bytes 20480
# Uses the exporter's private #encode API, so SDK upgrades may require changes.

require "bundler/setup"
require "opentelemetry/sdk"
require "opentelemetry/exporter/otlp"
require "optparse"
require "zlib"
require_relative "../lib/buildkite/test_collector"
require_relative "../lib/buildkite/test_collector/rspec_plugin/trace"

def exception_events_and_status(i, timestamp:, backtrace_lines:, message_bytes:, failures:)
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
    failure_reason: "Failure/Error: expected x got y ##{i}" * (failures > 1 ? 100 : 1),
    failure_expanded: Array.new(failures) { { expanded: [message], backtrace: backtrace } },
  )
  events = trace.otel_exception_events.map do |attributes|
    OpenTelemetry::SDK::Trace::Event.new("exception", attributes, timestamp)
  end
  [events, OpenTelemetry::Trace::Status.error(trace.otel_failure_reason)]
end

def span_data(i, failing:, backtrace_lines:, message_bytes:, failures: 1, description_bytes: nil)
  scope = "Some::Deeply::Nested::ServiceObject with a long context description ##{i}"
  name = "does the thing when the other thing is configured correctly ##{i}"
  if description_bytes
    # A generated group description (e.g. an inspected fixture) grows the
    # scope, the suite name and the full description together.
    scope = ("#{scope} " * (description_bytes.fdiv(scope.bytesize + 1).ceil)).byteslice(0, description_bytes - name.bytesize - 1)
  end
  attributes = {
    "buildkite.execution.via" => "otlp",
    "buildkite.run_key" => "83d96bfd-2388-4508-a8eb-070df6648da8",
    "test.case.result.status" => failing ? "fail" : "pass",
    "buildkite.test.scope" => scope,
    "buildkite.test.name" => name,
    "test.case.name" => "#{scope} #{name}",
    "test.suite.name" => scope,
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
  attributes.transform_values! { |value| Buildkite::TestCollector::OTel.send(:truncate_attribute_value, value) }
  timestamp = 1_700_000_000_000_000_000
  events, status = if failing
    exception_events_and_status(
      i, timestamp: timestamp, backtrace_lines: backtrace_lines, message_bytes: message_bytes, failures: failures,
    )
  else
    [[], OpenTelemetry::Trace::Status.unset]
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
    attributes.size, events.size, 0, timestamp, timestamp + 50_000_000,
    attributes, [], events, resource,
    OpenTelemetry::SDK::InstrumentationScope.new("buildkite-test-collector", Buildkite::TestCollector::VERSION),
    OpenTelemetry::Trace.generate_span_id, OpenTelemetry::Trace.generate_trace_id,
    OpenTelemetry::Trace::TraceFlags::SAMPLED, OpenTelemetry::Trace::Tracestate::DEFAULT,
  )
end

def span_overhead_bytes(exporter, span)
  event_attribute_bytes = span.events.sum { |event| event.attributes.values.sum(&:bytesize) }
  exporter.send(:encode, [span]).bytesize - event_attribute_bytes - span.status.description.to_s.bytesize
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
  [
    [false, 0, 0], [true, 30, 1_024], [true, 60, 4 * 1_024], [true, 100, 10 * 1_024], [true, 170, 10 * 1_024],
    [true, 170, 10 * 1_024, 100], [true, 1, 160, 100],
    [true, 170, 10 * 1_024, 100, 4 * 1_024],
  ]
end
# Ignore process-wide OTLP credentials and certificates so this offline
# measurement also works in suites configured for another destination.
exporter = OpenTelemetry::Exporter::OTLP::Exporter.new(
  endpoint: "http://127.0.0.1:1/v1/traces",
  headers: {},
  certificate_file: nil,
  client_certificate_file: nil,
  client_key_file: nil,
  ssl_verify_mode: OpenSSL::SSL::VERIFY_PEER,
  compression: "gzip",
  timeout: 10,
)
puts "RSpec limits applied; limits: raw <= 8192 KiB, gzip <= 900 KiB"
rows.each do |failing, lines, bytes, failures, description_bytes|
  failures ||= 1
  spans = Array.new(options[:spans]) do |i|
    span_data(i, failing: failing, backtrace_lines: lines, message_bytes: bytes, failures: failures, description_bytes: description_bytes)
  end
  raw = exporter.send(:encode, spans)
  gzip = Zlib.gzip(raw)
  printf "%d spans failing=%-5s backtrace_lines=%3d message_bytes=%5d failures=%3d%s: raw=%7.1f KiB gzip=%6.1f KiB%s%s\n",
    options[:spans], failing, lines, bytes, failures, description_bytes ? " description_bytes=#{description_bytes}" : "",
    raw.bytesize / 1024.0, gzip.bytesize / 1024.0,
    raw.bytesize > 8 * 1024 * 1024 ? " OVER decoded limit" : "",
    gzip.bytesize > 900 * 1024 ? " OVER gzip limit" : ""
  if description_bytes
    capped = spans.first.attributes.values.count { |value| value.is_a?(String) && value.end_with?(Buildkite::TestCollector::OTel::TRUNCATION_MARKER) }
    overhead = spans.map { |span| span_overhead_bytes(exporter, span) }.max
    puts "  #{capped} attribute values at the #{Buildkite::TestCollector::OTel::ATTRIBUTE_VALUE_MAX_BYTES}-byte cap; " \
      "maximum single-span protobuf overhead (including resource): #{overhead} bytes"
    if options[:spans] <= Buildkite::TestCollector::OTel::TEST_SPAN_MAX_EXPORT_BATCH_SIZE_LIMIT
      abort "long-description request exceeds decoded limit" if raw.bytesize > 8 * 1024 * 1024
    end
  elsif failures == 100
    overhead = spans.map { |span| span_overhead_bytes(exporter, span) }.max
    puts "  maximum single-span protobuf overhead (including resource): #{overhead} bytes"
    unreserved_overhead = spans.map do |span|
      details = span.events.reject { |event| event.attributes["exception.message"]&.end_with?("failures omitted by buildkite-test_collector") }
      event_attribute_bytes = details.sum { |event| event.attributes.values.sum(&:bytesize) }
      framing_bytes = [details.length - 1, 0].max * Buildkite::TestCollector::RSpecPlugin::Trace::EXCEPTION_EVENT_FRAMING_BYTES
      exporter.send(:encode, [span]).bytesize - event_attribute_bytes - framing_bytes - span.status.description.to_s.bytesize
    end.max
    puts "  overhead after additional-event reservations (including omission): #{unreserved_overhead} bytes"
    abort "representative span exceeds 2 KiB overhead allowance" if unreserved_overhead > 2_048
    trace_class = Buildkite::TestCollector::RSpecPlugin::Trace
    bound = options[:spans] * (trace_class::EXCEPTION_EVENTS_MAX_BYTES + trace_class::STATUS_DESCRIPTION_MAX_BYTES + 2_048)
    abort "aggregate-failure request exceeds its span-budget bound" if raw.bytesize > bound
    if options[:spans] <= Buildkite::TestCollector::OTel::TEST_SPAN_MAX_EXPORT_BATCH_SIZE_LIMIT
      abort "aggregate-failure request exceeds decoded limit" if raw.bytesize > 8 * 1024 * 1024
    end
  end
end
