# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Forwards the finished child spans of test spans to the child span
      # processor. While a test runs, its children are held back so they can
      # be stamped with the test's result once RSpec settles it; the server
      # can then sample passing tests' children and keep every failing one.
      # The hold is bounded across all tests: past the bound, children are
      # forwarded at once without a stamp, so the buffer never blocks a test
      # or costs a span.
      class ChildSpanForwarder
        # The held children of a running test, and its result once released.
        # Children still in flight keep a released test alive, so those that
        # finish after it are stamped too.
        Test = Struct.new(:running, :held, :result)
        private_constant :Test

        # For children of a test that is not held: one forgotten after it
        # finished (async work outliving its example) or never started.
        UNHELD = Test.new(false, [].freeze, nil).freeze
        private_constant :UNHELD

        # Stamps the result on the exported copy of a finished span, which
        # the SDK no longer lets us change. The batch processor needs only
        # these two methods, and calls to_span_data on its export thread.
        StampedSpan = Struct.new(:span, :result) do
          def context
            span.context
          end

          def to_span_data
            data = span.to_span_data
            attributes = data.attributes || {}
            # The exporter derives dropped_attributes_count from this total,
            # which must not fall below the number of attributes sent.
            data.total_recorded_attributes += 1 unless attributes.key?(CHILD_RESULT_ATTRIBUTE)
            data.attributes = attributes.merge(CHILD_RESULT_ATTRIBUTE => result).freeze
            data
          end
        end
        private_constant :StampedSpan

        def initialize(
          processor,
          context_key:,
          span_filter: nil,
          max_held_spans: CHILD_SPAN_MAX_QUEUE_SIZE - CHILD_SPAN_MAX_EXPORT_BATCH_SIZE
        )
          @processor = processor
          @context_key = context_key
          @span_filter = span_filter && SpanFilter.new(span_filter)
          @max_held_spans = max_held_spans
          @spans = {}
          @populated_phases = {}
          @tests = {}
          @held_count = 0
          @mutex = Mutex.new
          @pid = Process.pid
          @active = true
        end

        # Starts holding the children of the test span with this trace ID.
        #
        # Examples run one at a time, each finished before the next starts, so
        # a test still running here never will be: an around hook ran the
        # example more than once (as rspec-retry does) and only the last
        # attempt is reported. Its result is not coming, so its children are
        # exported unstamped rather than held until shutdown.
        def test_started(trace_id)
          locked do
            next unless @active

            @tests.each_value do |orphan|
              orphan.running = false
              release(orphan)
            end
            @tests.clear
            @tests[trace_id] = Test.new(true, [], nil)
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not hold OpenTelemetry child spans: #{e.class}: #{e.message}"
        end

        # Forwards the test's held children stamped with its result ("pass"
        # or "fail"; any other result is forwarded unstamped), along with any
        # still in flight as they finish.
        def test_finished(trace_id, result)
          result = nil unless STAMPED_RESULTS.include?(result)
          locked do
            test = @tests.delete(trace_id)
            next unless @active && test

            test.running = false
            test.result = result
            release(test)
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not export OpenTelemetry child spans: #{e.class}: #{e.message}"
        end

        def on_start(span, parent_context)
          test_span_trace_id = parent_context.value(@context_key)
          return unless test_span_trace_id
          return unless test_span_trace_id == span.context.trace_id

          parent = OpenTelemetry::Trace.current_span(parent_context)
          locked do
            next unless @active

            @spans[span] = @tests.fetch(test_span_trace_id, UNHELD)
            # A child that starts after its phase finished (async work from a
            # hook) must not re-add the phase, or it would live until shutdown.
            @populated_phases[parent] = true if @spans.key?(parent) && phase_span?(parent)
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not track OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        # Without a filter, a span is accepted and held or queued under one
        # lock, so shutdown cannot deactivate the forwarder in between and
        # lose it. A filter is caller code and runs outside the lock, so a
        # slow filter cannot stall other spans and one that finishes a span
        # cannot deadlock; a span still in its filter when shutdown runs is
        # dropped.
        def on_finish(span)
          unless @span_filter
            locked do
              next unless @active && (test = @spans.delete(span))

              forward(span, test) unless empty_phase?(span)
            end
            return
          end

          test = locked do
            next unless @active && (test = @spans.delete(span))
            next test unless phase_span?(span)

            # Phase spans are structure the UI relies on; the filter never sees them.
            forward(span, test) unless empty_phase?(span)
            nil
          end
          return unless test

          retained = @span_filter.retain?(span)
          locked do
            forward(span, test) if @active && retained
          end
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not export OpenTelemetry child span: #{e.class}: #{e.message}"
        end

        # Held children are not flushed: they wait for their test's result,
        # which arrives when the example finishes.
        def force_flush(timeout: nil)
          active = locked { @active }
          return success unless active

          @processor.force_flush(timeout: timeout)
        rescue Exception => e # rubocop:disable Lint/RescueException
          ExceptionHandling.reraise_fatal(e)
          warn "[buildkite-test_collector] Could not flush OpenTelemetry child spans: #{e.class}: #{e.message}"
          OpenTelemetry::SDK::Trace::Export::FAILURE
        end

        # Children still held have no result coming, so they are forwarded
        # unstamped rather than lost. The processor itself is left running:
        # the collector shuts it down after this, flushing them.
        def shutdown(timeout: nil)
          locked do
            @active = false
            @tests.each_value { |test| release(test) }
          ensure
            @spans.clear
            @populated_phases.clear
            @tests.clear
            @held_count = 0
          end
          success
        end

        private

        def locked
          @mutex.synchronize do
            reset_on_fork
            yield
          end
        end

        # Called under @mutex. A forked process inherits the parent's held
        # and in-flight children, which the parent exports itself; mirrors
        # BatchSpanProcessor#reset_on_fork, which drops its inherited queue.
        def reset_on_fork
          return if @pid == Process.pid

          @pid = Process.pid
          @spans.clear
          @populated_phases.clear
          @tests.clear
          @held_count = 0
        end

        # Called under @mutex with a finished child to export: holds it while
        # its test runs and the hold has room, otherwise queues it, stamped if
        # its test's result is known.
        def forward(span, test)
          if test.running && @held_count < @max_held_spans
            test.held << span
            @held_count += 1
          else
            @processor.on_finish(stamp(span, test.result))
          end
        end

        # Called under @mutex.
        def release(test)
          held = test.held
          test.held = []
          @held_count -= held.size
          held.each { |span| @processor.on_finish(stamp(span, test.result)) }
        end

        def stamp(span, result)
          result ? StampedSpan.new(span, result) : span
        end

        # Called under @mutex. A phase span that grouped nothing and did not
        # fail says nothing the test span does not, so an uninstrumented
        # suite exports no child spans at all.
        def empty_phase?(span)
          return false unless phase_span?(span)

          !@populated_phases.delete(span) && span.status.code == OpenTelemetry::Trace::Status::UNSET
        end

        # Only called with tracked spans, which have a name.
        def phase_span?(span)
          PHASE_SPAN_NAMES.value?(span.name)
        end

        def success
          OpenTelemetry::SDK::Trace::Export::SUCCESS
        end
      end
      private_constant :ChildSpanForwarder
    end
  end
end
