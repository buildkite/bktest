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
        failure = nil
        begin
          super
        rescue Exception => e # rubocop:disable Lint/RescueException
          # `skip` in a before hook raises to end the example early; RSpec
          # reports that as pending, not as a failure.
          failure = e unless RSpec::Core::Pending::SkipDeclaredInExample === e
          raise
        ensure
          Buildkite::TestCollector::OTel.detach_span(token)
          Buildkite::TestCollector::OTel.finish_phase_span(setup_span, failure)
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
        failure_before = exception
        failure_count_before = buildkite_failure_count(failure_before)
        failure = nil
        begin
          super
          failure = buildkite_teardown_failure(failure_before, failure_count_before)
        rescue Exception => e # rubocop:disable Lint/RescueException
          failure = e
          raise
        ensure
          Buildkite::TestCollector::OTel.detach_span(token)
          Buildkite::TestCollector::OTel.finish_phase_span(teardown_span, failure)
        end
      end

      # The body's failure, if any, is already the example's exception by
      # the time after hooks run; a setup failure leaves no body span.
      def buildkite_finish_body_span
        Buildkite::TestCollector::OTel.detach_span(@buildkite_body_token)
        Buildkite::TestCollector::OTel.finish_phase_span(@buildkite_body_span, exception)
      ensure
        @buildkite_body_span = nil
        @buildkite_body_token = nil
      end

      # After hooks rescue their own failures and add them to the example
      # (RSpec::Core::Hooks::AfterHook#run), so a teardown failure shows up
      # as a new or grown example exception rather than a raise. The first
      # failure replaces nil; a second wraps both in a MultipleExceptionError;
      # later ones are added to that same object, so the count is taken
      # before the hooks run.
      def buildkite_teardown_failure(failure_before, failure_count_before)
        failure_after = exception
        return if failure_after.nil?
        return if failure_after.equal?(failure_before) && buildkite_failure_count(failure_after) == failure_count_before

        failure_after.respond_to?(:all_exceptions) ? failure_after.all_exceptions.last : failure_after
      end

      def buildkite_failure_count(failure)
        return 0 if failure.nil?

        failure.respond_to?(:all_exceptions) ? failure.all_exceptions.size : 1
      end
    end
  end
end

RSpec::Core::Example.prepend(Buildkite::TestCollector::RSpecPlugin::PhaseSpans::ExampleHooks)
