# frozen_string_literal: true

require "rspec/core"

module Buildkite::TestCollector::RSpecPlugin
  # Splits each test.execution span into three phase spans: test.setup
  # (before hooks and let!), test.body (the example block), and test.teardown
  # (after hooks and mock verification). Each phase span is current while its
  # phase runs, so instrumentation spans nest under the phase they ran in and
  # the test span has a handful of children instead of every query and
  # request at once.
  #
  # RSpec::Core::Example#run calls run_before_example, then the example block,
  # then run_after_example in an ensure; these private methods are the
  # narrowest wrap points that see the failures each phase raises. Around
  # hooks run outside all three, so their own spans stay direct children of
  # the test span alongside the phases.
  module PhaseSpans
    module ExampleHooks
      private

      def run_before_example
        test_span = Buildkite::TestCollector::OTel.current_test_span
        return super unless test_span

        setup_span = Buildkite::TestCollector::OTel.start_phase_span(:setup, test_span)
        token = Buildkite::TestCollector::OTel.attach_span(setup_span)
        failures = []
        begin
          super
        rescue Exception => e # rubocop:disable Lint/RescueException
          # `skip` in a before hook raises to end the example early; RSpec
          # reports that as pending, not as a failure.
          failures = [e] unless RSpec::Core::Pending::SkipDeclaredInExample === e
          raise
        ensure
          Buildkite::TestCollector::OTel.detach_span(token)
          Buildkite::TestCollector::OTel.finish_phase_span(setup_span, failures)
        end

        # Left current until run_after_example, which runs even when the
        # example block raises.
        @buildkite_body_span = Buildkite::TestCollector::OTel.start_phase_span(:body, test_span)
        @buildkite_body_token = Buildkite::TestCollector::OTel.attach_span(@buildkite_body_span)
      end

      def run_after_example
        buildkite_finish_body_span
        test_span = Buildkite::TestCollector::OTel.current_test_span
        return super unless test_span

        teardown_span = Buildkite::TestCollector::OTel.start_phase_span(:teardown, test_span)
        token = Buildkite::TestCollector::OTel.attach_span(teardown_span)
        failures_before = buildkite_failures
        failures = []
        begin
          super
          failures = buildkite_failures - failures_before
        rescue Exception => e # rubocop:disable Lint/RescueException
          failures = [e]
          raise
        ensure
          Buildkite::TestCollector::OTel.detach_span(token)
          Buildkite::TestCollector::OTel.finish_phase_span(teardown_span, failures)
        end
      end

      # The body's failure, if any, is already the example's exception by
      # the time after hooks run; a setup failure leaves no body span.
      def buildkite_finish_body_span
        Buildkite::TestCollector::OTel.detach_span(@buildkite_body_token)
        Buildkite::TestCollector::OTel.finish_phase_span(@buildkite_body_span, buildkite_failures)
      ensure
        @buildkite_body_span = nil
        @buildkite_body_token = nil
      end

      # The example's failures so far, as a flat list. After hooks and mock
      # verification rescue their own failures and add them to the example
      # (RSpec::Core::Hooks::AfterHook#run, Example#verify_mocks) rather than
      # raising: the first replaces nil, a second wraps both in a
      # MultipleExceptionError, and later ones are added to that same object.
      # Snapshotting before and after the hooks and taking the difference
      # yields exactly the failures the teardown phase added.
      def buildkite_failures
        case exception
        when nil then []
        when RSpec::Core::MultipleExceptionError then exception.all_exceptions.dup
        else [exception]
        end
      end
    end
  end
end

RSpec::Core::Example.prepend(Buildkite::TestCollector::RSpecPlugin::PhaseSpans::ExampleHooks)
