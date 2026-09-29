# frozen_string_literal: true

module Buildkite::TestCollector::RSpecPlugin
  # Splits each test.execution span into three phase spans: test.setup
  # (before hooks and let!), test.body (the example block), and test.teardown
  # (after hooks and mock verification). Each phase span is current while its
  # phase runs, so instrumentation spans nest under the phase they ran in.
  #
  # RSpec::Core::Example#run calls run_before_example, then the example block,
  # then run_after_example in an ensure; these private methods are the
  # narrowest wrap points that see the failures each phase raises. Around
  # hooks run outside all three, so their own spans stay direct children of
  # the test span alongside the phases.
  module PhaseSpans
    private

    def run_before_example
      test_span = Buildkite::TestCollector::OTel.current_test_span
      return super unless test_span

      buildkite_phase(:setup, test_span) { super() }

      # Left current until run_after_example, which runs even when the
      # example block raises.
      @buildkite_body_span = Buildkite::TestCollector::OTel.start_phase_span(:body, test_span)
      @buildkite_body_token = Buildkite::TestCollector::OTel.attach_span(@buildkite_body_span)
    end

    def run_after_example
      buildkite_finish_body_span
      test_span = Buildkite::TestCollector::OTel.current_test_span
      return super unless test_span

      buildkite_phase(:teardown, test_span) do |failures|
        failures_before = buildkite_failures
        super()
        failures.concat(buildkite_failures - failures_before)
      end
    end

    # Anything the block raises is the phase's failure; a block that observes
    # failures without raising appends them to the yielded list.
    def buildkite_phase(phase, test_span)
      failures = []
      span = Buildkite::TestCollector::OTel.start_phase_span(phase, test_span)
      token = Buildkite::TestCollector::OTel.attach_span(span)
      yield failures
    rescue Exception => e # rubocop:disable Lint/RescueException
      # `skip` in a before hook ends the example early; RSpec reports that as
      # pending, not as a failure.
      failures << e unless e.is_a?(RSpec::Core::Pending::SkipDeclaredInExample)
      raise
    ensure
      Buildkite::TestCollector::OTel.detach_span(token)
      Buildkite::TestCollector::OTel.finish_phase_span(span, failures)
    end

    def buildkite_finish_body_span
      Buildkite::TestCollector::OTel.detach_span(@buildkite_body_token)
      Buildkite::TestCollector::OTel.finish_phase_span(@buildkite_body_span, buildkite_failures)
    ensure
      @buildkite_body_span = nil
      @buildkite_body_token = nil
    end

    # After hooks and mock verification add their failures to the example
    # rather than raising: the first replaces nil, a second wraps both in a
    # MultipleExceptionError, and later ones are added to that same object.
    def buildkite_failures
      case exception
      when nil then []
      when RSpec::Core::MultipleExceptionError then exception.all_exceptions.dup
      else [exception]
      end
    end
  end
end

RSpec::Core::Example.prepend(Buildkite::TestCollector::RSpecPlugin::PhaseSpans)
