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

    # Byte limits include the marker; truncation preserves valid UTF-8.
    # Legacy JSON failure detail is unchanged.
    OTEL_EXCEPTION_MESSAGE_MAX_BYTES = 10_240
    OTEL_EXCEPTION_STACKTRACE_MAX_BYTES = 16_384
    OTEL_STATUS_DESCRIPTION_MAX_BYTES = 1_024
    OTEL_EXCEPTION_EVENTS_MAX_BYTES = 26 * 1_024
    # Reserve protobuf framing for each additional event (two fixed attribute
    # names, lengths, event name and timestamp). The first event and omission
    # summary fit in the batch's separate 2 KiB span-overhead allowance.
    OTEL_EXCEPTION_EVENT_OVERHEAD_BYTES = 128
    OTEL_EXCEPTION_MAX_EVENTS = 100
    OTEL_TRUNCATION_MARKER = Buildkite::TestCollector::OTel::TRUNCATION_MARKER

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
      otel_truncate(failure_reason, OTEL_STATUS_DESCRIPTION_MAX_BYTES) if failure_reason
    end

    def otel_exception_events
      failures = failure_expanded || []
      events = []
      remaining = OTEL_EXCEPTION_EVENTS_MAX_BYTES
      consumed = 0
      stopped = false
      failures.each do |failure|
        message = Array(failure[:expanded]).join("\n")
        stacktrace = Array(failure[:backtrace]).join("\n")
        attributes = {}
        attributes["exception.message"] = otel_truncate(message, OTEL_EXCEPTION_MESSAGE_MAX_BYTES) unless message.empty?
        attributes["exception.stacktrace"] = otel_truncate(stacktrace, OTEL_EXCEPTION_STACKTRACE_MAX_BYTES) unless stacktrace.empty?
        unless attributes.empty? || events.empty?
          remaining -= OTEL_EXCEPTION_EVENT_OVERHEAD_BYTES
          if remaining <= OTEL_TRUNCATION_MARKER.bytesize
            stopped = true
            break
          end
        end
        if attributes.values.sum(&:bytesize) > remaining
          attributes = otel_fit_event(attributes, remaining)
          stopped = true
          break if attributes.empty?
        else
          remaining -= attributes.values.sum(&:bytesize)
        end
        consumed += 1
        events << attributes unless attributes.empty?
        stopped ||= remaining <= OTEL_TRUNCATION_MARKER.bytesize || events.length == OTEL_EXCEPTION_MAX_EVENTS
        break if stopped
      end

      # RSpec supplies an Array. A sized lazy enumerable also gives an exact
      # count without traversing its tail; an unsized one must remain lazy.
      total = failures.size
      omitted = total.is_a?(Integer) ? total - consumed : (stopped ? nil : 0)
      if omitted.nil? || omitted.positive?
        if events.length == OTEL_EXCEPTION_MAX_EVENTS
          events.pop
          omitted += 1 if omitted
        end
        count = omitted ? "#{omitted} more" : "More"
        events << { "exception.message" => "#{count} failures omitted by buildkite-test_collector" }
      end
      events
    end

    private

    def otel_fit_event(attributes, budget)
      first_key, first_value = attributes.first
      # Keep the message intact if a truncated stacktrace can still carry a
      # marker plus a full UTF-8 character (at most four bytes).
      if attributes.length == 2 && budget - first_value.bytesize >= OTEL_TRUNCATION_MARKER.bytesize + 4
        return {
          first_key => first_value,
          "exception.stacktrace" => otel_truncate(attributes.fetch("exception.stacktrace"), budget - first_value.bytesize),
        }
      end

      if attributes.length == 2
        first_value += OTEL_TRUNCATION_MARKER
        budget = [budget, OTEL_EXCEPTION_MESSAGE_MAX_BYTES].min
      end
      value = otel_truncate(first_value, budget)
      return {} if value == OTEL_TRUNCATION_MARKER

      { first_key => value }
    end

    def otel_truncate(value, limit)
      Buildkite::TestCollector::OTel.truncate(value, limit)
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
