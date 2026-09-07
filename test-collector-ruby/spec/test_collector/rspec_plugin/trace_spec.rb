# frozen_string_literal: true

require "buildkite/test_collector/rspec_plugin/trace"

RSpec.describe Buildkite::TestCollector::RSpecPlugin::Trace do
  subject(:trace) do
    Buildkite::TestCollector::RSpecPlugin::Trace.new(
      example,
      history: history,
      tags: tags,
      location_prefix: location_prefix,
      external_id: external_id,
    )
  end

  let(:example) { double(id: "test for invalid character '\xC8'").as_null_object }
  let(:location_prefix) { nil }
  let(:external_id) { nil }

  let(:history) do
    {
      children: [
        {
          start_at: 347611.734956,
          detail: %{"query"=>"SELECT '\xC8'"}
        }
      ]
    }
  end

  let(:tags) { nil }

  describe '#as_hash' do
    describe "file_name" do
      let(:example) { fake_example(file_path: file_path) }
      let(:file_path) { "./spec/foo_spec.rb" }

      it "is set from example.file_path" do
        expect(trace.as_hash).to include(
          file_name: "./spec/foo_spec.rb",
          location: "./spec/foo_spec.rb:42",
        )
      end

      context "when location_prefix is provided" do
        let(:location_prefix) { "some/prefix" }

        it "prepends location_prefix to example.file_path" do
          expect(trace.as_hash).to include(
            file_name: "some/prefix/spec/foo_spec.rb",
            location: "some/prefix/spec/foo_spec.rb:42",
          )
        end
      end
    end

    it 'removes invalid UTF-8 characters from nested values' do
      history_json = trace.as_hash[:history].to_json

      expect(history_json).to include('query')
      expect(history_json).to be_valid_encoding
    end

    it 'does not alter data types which are not strings' do
      history_json = trace.as_hash[:history].to_json

      expect(history_json).to include('347611.734956')
    end

    context "with tags" do
      let(:tags) { { "hello" => "world" } }

      it "includes the tags" do
        expect(trace.as_hash[:tags]).to eq({ "hello" => "world" })
      end
    end

    context "with an external ID" do
      let(:external_id) { "019c8d97-f9ad-75a5-8173-dc6c1b54b901" }

      it "includes the external ID" do
        expect(trace.as_hash[:external_id]).to eq(external_id)
      end
    end

  end

  describe "#otel_attributes" do
    let(:example) { fake_example(file_path: "./spec/foo_spec.rb") }

    it "describes the test, using the same path as the legacy upload" do
      expect(trace.otel_attributes).to eq(
        "buildkite.test.scope" => example.example_group.metadata[:full_description],
        "buildkite.test.name" => example.description,
        "test.case.name" => example.full_description,
        "test.suite.name" => example.example_group.metadata[:full_description],
        "code.file.path" => "./spec/foo_spec.rb",
        "code.line.number" => 42,
      )
    end

    context "with an external ID" do
      let(:external_id) { "019c8d97-f9ad-75a5-8173-dc6c1b54b901" }

      it "identifies the matching Test Engine execution" do
        expect(trace.otel_attributes.fetch("buildkite.test.execution.external_id"))
          .to eq(external_id)
      end
    end

    context "with execution tags" do
      let(:tags) { { "team" => "platform" } }

      it "includes them as prefixed span attributes" do
        expect(trace.otel_attributes).to include("buildkite.tag.team" => "platform")
      end
    end

    context "when location_prefix is provided" do
      let(:location_prefix) { "some/prefix" }

      it "uses the prefixed path, matching the upload" do
        expect(trace.otel_attributes.fetch("code.file.path")).to eq("some/prefix/spec/foo_spec.rb")
        expect(trace.otel_attributes.fetch("code.file.path")).to eq(trace.as_hash[:file_name])
      end
    end

    context "when the example comes from a shared example group" do
      let(:example) do
        fake_example(
          id: "./spec/consumer_spec.rb[1:1]",
          location: "./spec/support/shared_examples.rb:8",
          metadata: {
            shared_group_inclusion_backtrace: [
              OpenStruct.new(inclusion_location: "./spec/consumer_spec.rb:17"),
            ],
          },
        )
      end

      it "uses the shared example call site" do
        expect(trace.otel_attributes).to include(
          "code.file.path" => "./spec/consumer_spec.rb",
          "code.line.number" => 17,
        )
      end
    end
  end

  describe "#otel_failure_reason and #otel_exception_events" do
    it "exposes the failure summary and one exception event per failure" do
      trace.failure_reason = "it broke"
      trace.failure_expanded = [
        { expanded: ["it broke"], backtrace: ["foo.rb:1", "foo.rb:9"] },
        { expanded: [], backtrace: [] },
      ]

      expect(trace.otel_failure_reason).to eq("it broke")
      expect(trace.otel_exception_events).to eq([
        {
          "exception.message" => "it broke",
          "exception.stacktrace" => "foo.rb:1\nfoo.rb:9",
        },
      ])
    end

    it "is empty when the test did not fail" do
      expect(trace.otel_failure_reason).to be_nil
      expect(trace.otel_exception_events).to eq([])
    end

    {
      "exception.message" => [:expanded, 10_240],
      "exception.stacktrace" => [:backtrace, 16_384],
      "status description" => [nil, 1_024],
    }.each do |field, (key, limit)|
      context "with #{field}" do
        let(:failure_value) do
          key ? trace.otel_exception_events.first.fetch(field) : trace.otel_failure_reason
        end

        it "preserves content at the #{limit}-character boundary" do
          original = "界" * limit
          trace.failure_reason = original
          trace.failure_expanded = [{ key => [original] }] if key

          expect(failure_value).to eq(original)
        end

        it "caps content at #{limit} characters, including the visible marker" do
          original = "😀" * (limit + 1)
          trace.failure_reason = original
          trace.failure_expanded = [{ key => [original] }] if key
          marker = "… [truncated by buildkite-test_collector]"

          value = failure_value

          expect(value.length).to eq(limit)
          expect(value).to eq("😀" * (limit - marker.length) + marker)
          expect(value).to be_valid_encoding
          expect(trace.failure_reason).to eq(original)
          expect(trace.failure_expanded).to eq([{ key => [original] }]) if key
        end

        it "replaces invalid UTF-8 before applying the limit" do
          original = "\xC8" + "a" * (limit - 1)
          trace.failure_reason = original
          trace.failure_expanded = [{ key => [original] }] if key

          expect(failure_value).to eq("�" + "a" * (limit - 1))
          expect(failure_value).to be_valid_encoding
        end
      end
    end

    it "keeps only the first 100 nonempty exception events" do
      trace.failure_expanded = [{ expanded: [] }] + Array.new(101) do |i|
        { expanded: ["failure #{i}"] }
      end

      expect(trace.otel_exception_events).to eq(
        Array.new(100) { |i| { "exception.message" => "failure #{i}" } },
      )
      expect(trace.failure_expanded.length).to eq(102)
    end

    it "keeps the default single-failure batch estimate below ingestion's decoded limit" do
      # Ingestion silently drops an entire request above 8 MiB decoded protobuf,
      # even when the gzip body fits at the edge. Allow ~2 KiB for attributes,
      # status and protobuf overhead per span. This is an ASCII, one-exception
      # estimate, not a byte guarantee for Unicode, many events or large tags.
      bytes_per_span = described_class::OTEL_EXCEPTION_MESSAGE_MAX_LENGTH +
        described_class::OTEL_EXCEPTION_STACKTRACE_MAX_LENGTH + 2 * 1_024
      batch_size = Buildkite::TestCollector::OTel::TEST_SPAN_MAX_EXPORT_BATCH_SIZE

      expect(batch_size * bytes_per_span).to be <= 8 * 1_024 * 1_024
    end
  end
end
