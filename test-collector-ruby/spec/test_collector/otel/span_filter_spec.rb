# frozen_string_literal: true

span_filter_class = Buildkite::TestCollector::OTel.const_get(:SpanFilter, false)

RSpec.describe span_filter_class do
  let(:span) { double("span") }

  it "asks the filter whether to retain each span" do
    filter = described_class.new(->(candidate) { candidate.equal?(span) })

    expect(filter.retain?(span)).to be(true)
    expect(filter.retain?(double("other span"))).to be(false)
  end

  it "retains spans when the filter fails, warning once" do
    filter = described_class.new(->(_span) { raise "filter failed" })

    expect { expect(filter.retain?(span)).to be(true) }
      .to output(/Could not filter OpenTelemetry child span, retaining it: RuntimeError: filter failed/).to_stderr
    expect { expect(filter.retain?(span)).to be(true) }.not_to output.to_stderr
  end

  it "retains spans when the filter cannot be called with a span" do
    filter = described_class.new(-> { false })

    expect { expect(filter.retain?(span)).to be(true) }
      .to output(/Could not filter OpenTelemetry child span, retaining it/).to_stderr
  end
end
