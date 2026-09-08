# frozen_string_literal: true

module Buildkite::TestCollector::RSpecPlugin
  class Trace < Buildkite::TestCollector::Trace
    attr_accessor :example, :failure_reason, :failure_expanded

    # Left open by the around hook; Reporter finishes it once RSpec
    # settles the example's result.
    attr_accessor :otel_span, :otel_end_timestamp

    attr_accessor :history
    attr_reader :tags
    attr_reader :location_prefix

    FILE_PATH_REGEX = /^(.*?\.(rb|feature))/

    EXCEPTION_MESSAGE_MAX_BYTES = 10_240
    EXCEPTION_STACKTRACE_MAX_BYTES = 16_384
    STATUS_DESCRIPTION_MAX_BYTES = 1_024
    EXCEPTION_EVENTS_MAX_BYTES = 26 * 1_024
    # Additional events need protobuf framing; the first event and omission
    # summary use the batch's separate 2 KiB span-overhead allowance.
    EXCEPTION_EVENT_FRAMING_BYTES = 128
    EXCEPTION_EVENT_COUNT_LIMIT = 100
    TRUNCATION_MARKER = Buildkite::TestCollector::OTel::TRUNCATION_MARKER

    def initialize(example, history:, failure_reason: nil, failure_expanded: [], tags: nil, location_prefix: nil, external_id: nil)
      @example = example
      @history = history
      @failure_reason = failure_reason
      @failure_expanded = failure_expanded
      @tags = tags
      @location_prefix = location_prefix
      @external_id = external_id
    end

    def result
      case example.execution_result.status
      when :passed; "passed"
      when :failed; "failed"
      when :pending; "skipped"
      end
    end

    # Read at reporter time, when the result is final.
    alias_method :otel_result, :result

    # What the span says about the test itself. OTel adds the submission
    # marker and run metadata that describe every test span.
    def otel_attributes
      attributes = {
        "buildkite.test.scope" => strip_invalid_utf8_chars(scope),
        "buildkite.test.name" => strip_invalid_utf8_chars(name),
        "test.case.name" => strip_invalid_utf8_chars(example.full_description),
        "test.suite.name" => strip_invalid_utf8_chars(scope),
        "code.file.path" => strip_invalid_utf8_chars(prepend_location_prefix(file_name)),
        "code.line.number" => source_line_number,
      }
      attributes["buildkite.test.execution.external_id"] = external_id if external_id
      prefix = Buildkite::TestCollector::OTel::TAG_ATTRIBUTE_PREFIX
      tags&.each do |key, value|
        attributes["#{prefix}#{key}"] = strip_invalid_utf8_chars(value.to_s)
      end
      attributes
    end

    def otel_failure_reason
      Buildkite::TestCollector::OTel.truncate_string(failure_reason, max_bytes: STATUS_DESCRIPTION_MAX_BYTES) if failure_reason
    end

    def otel_exception_events
      failures = failure_expanded || []
      events = []
      remaining_bytes = EXCEPTION_EVENTS_MAX_BYTES
      consumed_failures = 0
      limit_reached = false
      failures.each do |failure|
        attributes = exception_event_attributes(failure)
        unless attributes.empty? || events.empty?
          remaining_bytes -= EXCEPTION_EVENT_FRAMING_BYTES
          if remaining_bytes <= TRUNCATION_MARKER.bytesize
            limit_reached = true
            break
          end
        end
        event_bytes = attributes.values.sum(&:bytesize)
        if event_bytes > remaining_bytes
          attributes = fit_exception_event(attributes, max_bytes: remaining_bytes)
          limit_reached = true
          break if attributes.empty?
        else
          remaining_bytes -= event_bytes
        end
        consumed_failures += 1
        events << attributes unless attributes.empty?
        limit_reached ||= remaining_bytes <= TRUNCATION_MARKER.bytesize || events.length == EXCEPTION_EVENT_COUNT_LIMIT
        break if limit_reached
      end

      # RSpec supplies an Array. A sized lazy enumerable also gives an exact
      # count without traversing its tail; an unsized one must remain lazy.
      total_failures = failures.size
      omitted_failures = total_failures.is_a?(Integer) ? total_failures - consumed_failures : (limit_reached ? nil : 0)
      append_omission_event(events, omitted_failures)
    end

    private

    def exception_event_attributes(failure)
      message = Array(failure[:expanded]).join("\n")
      stacktrace = Array(failure[:backtrace]).join("\n")
      attributes = {}
      unless message.empty?
        attributes["exception.message"] = Buildkite::TestCollector::OTel.truncate_string(message, max_bytes: EXCEPTION_MESSAGE_MAX_BYTES)
      end
      unless stacktrace.empty?
        attributes["exception.stacktrace"] = Buildkite::TestCollector::OTel.truncate_string(stacktrace, max_bytes: EXCEPTION_STACKTRACE_MAX_BYTES)
      end
      attributes
    end

    def append_omission_event(events, omitted_failures)
      return events unless omitted_failures.nil? || omitted_failures.positive?

      if events.length == EXCEPTION_EVENT_COUNT_LIMIT
        events.pop
        omitted_failures += 1 if omitted_failures
      end
      count = omitted_failures ? "#{omitted_failures} more" : "More"
      events << { "exception.message" => "#{count} failures omitted by buildkite-test_collector" }
    end

    def fit_exception_event(attributes, max_bytes:)
      first_key, first_value = attributes.first
      # Keep the message intact if a truncated stacktrace can still carry a
      # marker plus a full UTF-8 character (at most four bytes).
      if attributes.length == 2 && max_bytes - first_value.bytesize >= TRUNCATION_MARKER.bytesize + 4
        return {
          first_key => first_value,
          "exception.stacktrace" => Buildkite::TestCollector::OTel.truncate_string(
            attributes.fetch("exception.stacktrace"), max_bytes: max_bytes - first_value.bytesize,
          ),
        }
      end

      if attributes.length == 2
        first_value += TRUNCATION_MARKER
        max_bytes = [max_bytes, EXCEPTION_MESSAGE_MAX_BYTES].min
      end
      value = Buildkite::TestCollector::OTel.truncate_string(first_value, max_bytes: max_bytes)
      return {} if value == TRUNCATION_MARKER

      { first_key => value }
    end

    # Shared examples report the location of the shared block, so use the call
    # site instead, the same way file_name does.
    def source_line_number
      source = shared_example? ? shared_example_call_location : example.location
      source[/:(\d+)\z/, 1]&.to_i
    end

    def scope
      example.example_group.metadata[:full_description]
    end

    def name
      example.description
    end

    def location
      example.location
    end

    def file_name
      @file_name ||= begin
        identifier_file_name = strip_invalid_utf8_chars(example.id)[FILE_PATH_REGEX]
        location_file_name = example.location[FILE_PATH_REGEX]

        if identifier_file_name != location_file_name
          # If the identifier and location files are not the same, we assume
          # that the test was run as part of a shared example. If this isn't the
          # case, then there's something we haven't accounted for
          if shared_example?
            # Taking the last frame in this backtrace will give us the original
            # entry point for the shared example
            shared_example_call_location[FILE_PATH_REGEX]
          else
            "Unknown"
          end
        else
          identifier_file_name
        end
      end
    end

    def shared_example?
      !example.metadata[:shared_group_inclusion_backtrace].empty?
    end

    def shared_example_call_location
      example.metadata[:shared_group_inclusion_backtrace].last.inclusion_location
    end
  end
end
