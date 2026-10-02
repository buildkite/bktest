# frozen_string_literal: true

require "opentelemetry/sdk"
require "opentelemetry/exporter/otlp"
require "timeout"

forwarder_class = Buildkite::TestCollector::OTel.const_get(:ChildSpanForwarder, false)

RSpec.describe forwarder_class do
  subject(:forwarder) { described_class.new(processor, context_key: context_key) }

  let(:success) { OpenTelemetry::SDK::Trace::Export::SUCCESS }
  let(:processor) do
    spy("execution child processor", on_finish: nil, force_flush: success, shutdown: success)
  end
  let(:context_key) { OpenTelemetry::Context.create_key("execution") }
  let(:test_span_trace_id) { "\1" * 16 }
  let(:execution_context) { OpenTelemetry::Context.empty.set_value(context_key, test_span_trace_id) }
  let(:span) { double("span", name: "GET", context: double("span context", trace_id: test_span_trace_id)) }

  it "forwards only spans from the execution trace" do
    unrelated_span = double("unrelated span")
    detached_span = double(
      "detached span",
      context: double("detached span context", trace_id: "\2" * 16),
    )

    forwarder.on_start(unrelated_span, OpenTelemetry::Context.empty)
    forwarder.on_finish(unrelated_span)
    forwarder.on_start(detached_span, execution_context)
    forwarder.on_finish(detached_span)
    forwarder.on_start(span, execution_context)
    forwarder.on_finish(span)

    expect(processor).to have_received(:on_finish).with(span).once
    expect(processor).not_to have_received(:on_finish).with(unrelated_span)
    expect(processor).not_to have_received(:on_finish).with(detached_span)
  end

  it "remembers an accepted span until it finishes" do
    forwarder.on_start(span, execution_context)

    forwarder.on_finish(span)
    forwarder.on_finish(span)

    expect(processor).to have_received(:on_finish).with(span).once
  end

  it "forwards only spans accepted by the configured filter" do
    rejected_span = double(
      "rejected span",
      name: "SELECT",
      context: double("rejected span context", trace_id: test_span_trace_id),
    )
    span_filter = ->(candidate) { candidate.equal?(span) }
    filtered_forwarder = described_class.new(
      processor,
      context_key: context_key,
      span_filter: span_filter,
    )

    filtered_forwarder.on_start(span, execution_context)
    filtered_forwarder.on_start(rejected_span, execution_context)
    filtered_forwarder.on_finish(span)
    filtered_forwarder.on_finish(rejected_span)

    expect(processor).to have_received(:on_finish).with(span).once
    expect(processor).not_to have_received(:on_finish).with(rejected_span)
    expect(filtered_forwarder.instance_variable_get(:@spans)).to be_empty
  end

  def phase_span(status: OpenTelemetry::Trace::Status.unset)
    double(
      "phase span",
      name: "test.body",
      status: status,
      context: double("phase span context", trace_id: test_span_trace_id),
    )
  end

  def context_under(parent_span)
    OpenTelemetry::Trace.context_with_span(parent_span, parent_context: execution_context)
  end

  it "forwards a phase span with children without consulting the filter" do
    populated_phase = phase_span
    filter_calls = []
    filtered_forwarder = described_class.new(
      processor,
      context_key: context_key,
      span_filter: ->(candidate) { filter_calls << candidate; false },
    )

    filtered_forwarder.on_start(populated_phase, execution_context)
    filtered_forwarder.on_start(span, context_under(populated_phase))
    filtered_forwarder.on_finish(span)
    filtered_forwarder.on_finish(populated_phase)

    expect(processor).to have_received(:on_finish).with(populated_phase).once
    expect(processor).not_to have_received(:on_finish).with(span)
    expect(filter_calls).to eq([span])
  end

  it "drops a phase span that grouped nothing and did not fail" do
    empty_phase = phase_span

    forwarder.on_start(empty_phase, execution_context)
    forwarder.on_finish(empty_phase)

    expect(processor).not_to have_received(:on_finish)
    expect(forwarder.instance_variable_get(:@spans)).to be_empty
  end

  it "forwards a phase span that failed even when it grouped nothing" do
    failed_phase = phase_span(status: OpenTelemetry::Trace::Status.error("boom"))

    forwarder.on_start(failed_phase, execution_context)
    forwarder.on_finish(failed_phase)

    expect(processor).to have_received(:on_finish).with(failed_phase).once
  end

  it "forgets a phase span's children once it finishes" do
    populated_phase = phase_span

    forwarder.on_start(populated_phase, execution_context)
    forwarder.on_start(span, context_under(populated_phase))
    forwarder.on_finish(span)
    forwarder.on_finish(populated_phase)

    expect(forwarder.instance_variable_get(:@populated_phases)).to be_empty
  end

  it "does not retain a finished phase span when a child starts under it late" do
    finished_phase = phase_span

    forwarder.on_start(finished_phase, execution_context)
    forwarder.on_finish(finished_phase)
    forwarder.on_start(span, context_under(finished_phase))
    forwarder.on_finish(span)

    expect(processor).to have_received(:on_finish).with(span).once
    expect(forwarder.instance_variable_get(:@populated_phases)).to be_empty
  end

  it "runs the filter without holding the lock" do
    mutex_owned = nil
    filtered_forwarder = described_class.new(
      processor,
      context_key: context_key,
      span_filter: ->(_span) { mutex_owned = filtered_forwarder.instance_variable_get(:@mutex).owned? },
    )
    filtered_forwarder.on_start(span, execution_context)

    filtered_forwarder.on_finish(span)

    expect(mutex_owned).to be(false)
  end

  # The filter runs unlocked, so shutdown can complete while a span is still
  # being filtered; that span must not reach the processor afterwards.
  it "drops a span whose filter is still running when shutdown happens" do
    filtered_forwarder = nil
    filtered_forwarder = described_class.new(
      processor,
      context_key: context_key,
      span_filter: lambda do |_span|
        filtered_forwarder.shutdown
        true
      end,
    )
    filtered_forwarder.on_start(span, execution_context)

    filtered_forwarder.on_finish(span)

    expect(processor).not_to have_received(:on_finish)
  end

  # Stands in for an instrumented call inside the filter, whose span finishes
  # on this thread while the filter is still running. Thread.current[] is
  # fiber-local, so the guard must also hold when that call runs in a fiber.
  {
    "directly" => ->(&block) { block.call },
    "in a fiber" => ->(&block) { Fiber.new(&block).resume },
  }.each do |how, run|
    it "retains spans the filter itself finishes #{how} instead of re-entering the filter" do
      nested_span = double(
        "nested span",
        name: "GET",
        context: double("nested span context", trace_id: test_span_trace_id),
      )
      filter_calls = 0
      filtered_forwarder = nil
      span_filter = lambda do |_span|
        filter_calls += 1
        # Each fiber has its own stack, so unbounded re-entry would hang rather
        # than raise SystemStackError; bail out early instead.
        raise "filter re-entered" if filter_calls > 1

        run.call do
          filtered_forwarder.on_start(nested_span, execution_context)
          filtered_forwarder.on_finish(nested_span)
        end
        false
      end
      filtered_forwarder = described_class.new(
        processor,
        context_key: context_key,
        span_filter: span_filter,
      )
      filtered_forwarder.on_start(span, execution_context)

      filtered_forwarder.on_finish(span)

      expect(filter_calls).to eq(1)
      expect(processor).to have_received(:on_finish).with(nested_span).once
      expect(processor).not_to have_received(:on_finish).with(span)
      expect(Thread.current.thread_variable_get(:buildkite_test_collector_span_filter_running)).to be_nil
    end
  end

  # Shutdown takes the same lock, so accepting and enqueueing a span under a
  # single acquisition means deactivation cannot slip in between and lose it.
  it "accepts and enqueues a span under one lock when no filter is configured" do
    mutex = forwarder.instance_variable_get(:@mutex)
    acquisitions = 0
    allow(mutex).to receive(:synchronize).and_wrap_original do |original, &block|
      acquisitions += 1
      original.call(&block)
    end
    mutex_owned = false
    allow(processor).to receive(:on_finish) { mutex_owned = mutex.owned? }
    forwarder.on_start(span, execution_context)
    acquisitions = 0

    forwarder.on_finish(span)

    expect(processor).to have_received(:on_finish).with(span).once
    expect(mutex_owned).to be(true)
    expect(acquisitions).to eq(1)
  end

  it "becomes inert without shutting down the child processor" do
    forwarder.on_start(span, execution_context)

    expect(forwarder.force_flush(timeout: 5)).to eq(success)
    expect(forwarder.shutdown).to eq(success)
    forwarder.on_finish(span)

    expect(processor).to have_received(:force_flush).with(timeout: 5).once
    expect(processor).not_to have_received(:shutdown)
    expect(processor).not_to have_received(:on_finish)
  end

  it "does not block span completion or deactivation during a flush" do
    flush_started = Queue.new
    release_flush = Queue.new
    flush_thread = nil
    allow(processor).to receive(:force_flush) do
      flush_started << true
      release_flush.pop
      success
    end
    forwarder.on_start(span, execution_context)

    flush_thread = Thread.new { forwarder.force_flush }
    flush_started.pop
    Timeout.timeout(1) do
      forwarder.on_finish(span)
      forwarder.shutdown
    end
    release_flush << true

    expect(flush_thread.value).to eq(success)
    expect(processor).to have_received(:on_finish).with(span).once
  ensure
    release_flush&.push(true) if flush_thread&.alive?
    flush_thread&.join
  end

  it "does not expose child processor failures to the suite" do
    allow(processor).to receive(:on_finish).and_raise("queue failed")
    allow(processor).to receive(:force_flush).and_raise("flush failed")
    forwarder.on_start(span, execution_context)

    expect { forwarder.on_finish(span) }
      .to output(/Could not export OpenTelemetry child span: RuntimeError: queue failed/).to_stderr
    expect { expect(forwarder.force_flush).to eq(OpenTelemetry::SDK::Trace::Export::FAILURE) }
      .to output(/Could not flush OpenTelemetry child spans: RuntimeError: flush failed/).to_stderr
  end

  describe "result stamp" do
    let(:exporter) { OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new }
    let(:max_held_spans) { 3 }
    let(:stamping_forwarder) do
      described_class.new(
        OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter),
        context_key: context_key,
        max_held_spans: max_held_spans,
      )
    end
    let(:provider) do
      OpenTelemetry::SDK::Trace::TracerProvider.new.tap { |p| p.add_span_processor(stamping_forwarder) }
    end
    let(:tracer) { provider.tracer("app") }
    # From its own provider, as the collector's private test span provider is.
    let(:test_span) do
      OpenTelemetry::SDK::Trace::TracerProvider.new.tracer("root")
        .start_span("test.execution", with_parent: OpenTelemetry::Context.empty)
    end

    after { provider.shutdown }

    def in_test(&block)
      OpenTelemetry::Context.with_value(context_key, test_span.context.trace_id) do
        OpenTelemetry::Trace.with_span(test_span, &block)
      end
    end

    def stamps
      exporter.finished_spans.to_h { |span| [span.name, span.attributes&.[]("buildkite.test.result")] }
    end

    def forgets_everything
      expect(stamping_forwarder.instance_variable_get(:@tests)).to be_empty
      expect(stamping_forwarder.instance_variable_get(:@spans)).to be_empty
      expect(stamping_forwarder.instance_variable_get(:@held_count)).to eq(0)
    end

    it "holds a passing test's children, phase spans included, until its result and stamps them pass" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test do
        tracer.in_span("test.body") { tracer.in_span("SELECT") { nil } }
      end

      expect(exporter.finished_spans).to be_empty

      stamping_forwarder.test_finished(test_span.context.trace_id, "pass")

      expect(stamps).to eq("SELECT" => "pass", "test.body" => "pass")
      # The stamp is counted as a recorded attribute, so the exporter reports
      # no dropped attributes rather than failing to encode a negative count.
      request = Opentelemetry::Proto::Collector::Trace::V1::ExportTraceServiceRequest.decode(
        OpenTelemetry::Exporter::OTLP::Common.as_encoded_etsr(exporter.finished_spans)
      )
      encoded = request.resource_spans.flat_map(&:scope_spans).flat_map(&:spans)
      expect(encoded.map(&:dropped_attributes_count)).to all(eq(0))
      forgets_everything
    end

    # The hold is bounded so thousands of children cannot pile up in memory;
    # past the bound a failing test's children must still all arrive.
    it "exports a failing test's children past the cap at once and unstamped, never dropping them" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test do
        5.times { |index| tracer.in_span("child-#{index}") { nil } }
      end

      expect(stamps).to eq("child-3" => nil, "child-4" => nil)

      stamping_forwarder.test_finished(test_span.context.trace_id, "fail")

      expect(stamps).to eq(
        "child-0" => "fail", "child-1" => "fail", "child-2" => "fail", "child-3" => nil, "child-4" => nil,
      )
      forgets_everything
    end

    it "frees the hold for the next test once a full one is released" do
      [["first", "pass"], ["second", "fail"]].each do |prefix, result|
        span = OpenTelemetry::SDK::Trace::TracerProvider.new.tracer("root")
          .start_span("test.execution", with_parent: OpenTelemetry::Context.empty)
        stamping_forwarder.test_started(span.context.trace_id)
        OpenTelemetry::Context.with_value(context_key, span.context.trace_id) do
          OpenTelemetry::Trace.with_span(span) do
            max_held_spans.times { |index| tracer.in_span("#{prefix}-#{index}") { nil } }
          end
        end
        stamping_forwarder.test_finished(span.context.trace_id, result)
      end

      expect(stamps.values).to eq(%w[pass] * max_held_spans + %w[fail] * max_held_spans)
    end

    it "stamps children that finish after their test ends, then forgets the test" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      late = in_test { tracer.start_span("late") }

      stamping_forwarder.test_finished(test_span.context.trace_id, "fail")
      expect(exporter.finished_spans).to be_empty
      late.finish

      expect(stamps).to eq("late" => "fail")
      forgets_everything
    end

    it "exports a child that starts after its test was forgotten at once, unstamped" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      stamping_forwarder.test_finished(test_span.context.trace_id, "pass")

      in_test { tracer.in_span("after the test") { nil } }

      expect(stamps).to eq("after the test" => nil)
      forgets_everything
    end

    it "leaves the children of a skipped test unstamped" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test { tracer.in_span("child") { nil } }

      stamping_forwarder.test_finished(test_span.context.trace_id, "skipped")

      expect(stamps).to eq("child" => nil)
    end

    it "holds and stamps children that the filter retains" do
      filtered = described_class.new(
        OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter),
        context_key: context_key,
        span_filter: ->(span) { span.name != "dropped" },
      )
      provider.add_span_processor(filtered)
      stamping_forwarder.shutdown
      filtered.test_started(test_span.context.trace_id)
      in_test do
        tracer.in_span("kept") { nil }
        tracer.in_span("dropped") { nil }
      end

      expect(exporter.finished_spans).to be_empty

      filtered.test_finished(test_span.context.trace_id, "fail")

      expect(stamps).to eq("kept" => "fail")
      expect(filtered.instance_variable_get(:@tests)).to be_empty
    end

    # The worker exports only once the queue passes a batch, so up to a batch
    # can sit queued when a test's hold is released; with the hold bounded by
    # the rest of the queue, that burst cannot evict them.
    it "releases a full hold into a batch processor without evicting a batch already queued" do
      bsp_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
      metrics_reporter = spy("metrics reporter")
      bsp = OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(
        bsp_exporter,
        max_queue_size: 4,
        max_export_batch_size: 2,
        start_thread_on_boot: false,
        metrics_reporter: metrics_reporter,
      )
      # As configure! bounds it: the queue size less a batch.
      bsp_forwarder = described_class.new(bsp, context_key: context_key, max_held_spans: 4 - 2)
      provider.add_span_processor(bsp_forwarder)
      stamping_forwarder.shutdown

      %w[first second].each do |prefix|
        span = OpenTelemetry::SDK::Trace::TracerProvider.new.tracer("root")
          .start_span("test.execution", with_parent: OpenTelemetry::Context.empty)
        bsp_forwarder.test_started(span.context.trace_id)
        OpenTelemetry::Context.with_value(context_key, span.context.trace_id) do
          OpenTelemetry::Trace.with_span(span) { 2.times { |index| tracer.in_span("#{prefix}-#{index}") { nil } } }
        end
        bsp_forwarder.test_finished(span.context.trace_id, "fail")
      end
      bsp.force_flush

      expect(metrics_reporter).not_to have_received(:add_to_counter).with("otel.bsp.dropped_spans", any_args)
      expect(bsp_exporter.finished_spans.map(&:name)).to eq(%w[first-0 first-1 second-0 second-1])
      expect(bsp_exporter.finished_spans.map { |span| span.attributes["buildkite.test.result"] }).to all(eq("fail"))
    ensure
      bsp&.shutdown
    end

    # rspec-retry runs the collector's around hook once per attempt, but only
    # the last attempt is reported, so earlier attempts never finish.
    it "exports an unfinished test's children unstamped when the next test starts" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test { tracer.in_span("first attempt") { nil } }

      stamping_forwarder.test_started("\3" * 16)

      expect(stamps).to eq("first attempt" => nil)
      expect(stamping_forwarder.instance_variable_get(:@tests).keys).to eq(["\3" * 16])
      expect(stamping_forwarder.instance_variable_get(:@held_count)).to eq(0)
    end

    # The forked process runs the inherited at_exit shutdown; the parent
    # exports its own held children, so the child must not export them too.
    it "leaves a forked process's inherited held children to the parent, exporting only its own" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test { tracer.in_span("parent child") { nil } }

      reader, writer = IO.pipe
      pid = fork do
        reader.close
        in_test { tracer.in_span("forked child") { nil } }
        stamping_forwarder.shutdown
        writer.write(Marshal.dump(stamps))
        exit!(0)
      end
      writer.close
      forked_stamps = Marshal.load(reader.read)
      Process.wait(pid)

      expect(forked_stamps).to eq("forked child" => nil)
      stamping_forwarder.test_finished(test_span.context.trace_id, "fail")
      expect(stamps).to eq("parent child" => "fail")
    end

    it "exports held children unstamped at shutdown rather than losing them" do
      stamping_forwarder.test_started(test_span.context.trace_id)
      in_test { tracer.in_span("child") { nil } }

      stamping_forwarder.shutdown

      expect(stamps).to eq("child" => nil)
      forgets_everything
    end
  end

  it "keeps the test span when the child queue overflows" do
    root_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    child_exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
    root_processor = OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(
      root_exporter,
      max_queue_size: 4,
      max_export_batch_size: 4,
      start_thread_on_boot: false,
    )
    child_processor = OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(
      child_exporter,
      max_queue_size: 4,
      max_export_batch_size: 4,
      start_thread_on_boot: false,
    )
    root_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    root_provider.add_span_processor(root_processor)
    suite_provider = OpenTelemetry::SDK::Trace::TracerProvider.new
    suite_provider.add_span_processor(
      described_class.new(child_processor, context_key: context_key)
    )
    root_tracer = root_provider.tracer("root")
    suite_tracer = suite_provider.tracer("suite")

    root = root_tracer.start_span("test.execution")
    OpenTelemetry::Context.with_value(context_key, root.context.trace_id) do
      OpenTelemetry::Trace.with_span(root) do
        10.times { |index| suite_tracer.in_span("child-#{index}") { nil } }
      end
    end
    root.finish
    root_processor.force_flush
    child_processor.force_flush

    expect(root_exporter.finished_spans.map(&:name)).to contain_exactly("test.execution")
    expect(child_exporter.finished_spans.size).to eq(4)
  ensure
    suite_provider&.shutdown
    root_provider&.shutdown
    child_processor&.shutdown
  end
end
