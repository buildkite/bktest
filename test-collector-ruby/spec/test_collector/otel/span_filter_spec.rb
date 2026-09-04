# frozen_string_literal: true

span_filter_class = Buildkite::TestCollector::OTel.const_get(:SpanFilter, false)

RSpec.describe span_filter_class do
  let(:span) { double("span") }

  it "retains every span when no filter is configured" do
    expect(described_class.from(nil).retain?(span)).to be(true)
  end

  it "asks the filter whether to retain each span" do
    filter = described_class.from(->(candidate) { candidate.equal?(span) })

    expect(filter.retain?(span)).to be(true)
    expect(filter.retain?(double("other span"))).to be(false)
  end

  it "retains spans when the filter fails, warning once" do
    filter = described_class.from(->(_span) { raise "filter failed" })

    expect { expect(filter.retain?(span)).to be(true) }
      .to output(/Could not filter OpenTelemetry child span, retaining it: RuntimeError: filter failed/).to_stderr
    expect { expect(filter.retain?(span)).to be(true) }.not_to output.to_stderr
  end

  it "retains spans when the filter cannot be called with a span" do
    [true, -> { false }].each do |unusable|
      filter = described_class.from(unusable)

      expect { expect(filter.retain?(span)).to be(true) }
        .to output(/Could not filter OpenTelemetry child span, retaining it/).to_stderr
    end
  end
end
