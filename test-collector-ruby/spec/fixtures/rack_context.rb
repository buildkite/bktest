# frozen_string_literal: true

require "json"
require "capybara"
require "rackup"
require "opentelemetry/sdk"
require "opentelemetry-instrumentation-rack"
require "opentelemetry-instrumentation-net_http"
require "webmock"
require "buildkite/test_collector"
require "rspec/core"
require "rspec/expectations"

include WebMock::API
WebMock.enable!
WebMock.disable_net_connect!(allow_localhost: true)
exported = []
lock = Mutex.new
endpoint = "https://example.invalid/v1/traces"
stub_request(:post, endpoint).to_return do |request|
  payload = Opentelemetry::Proto::Collector::Trace::V1::ExportTraceServiceRequest.decode(Zlib.gunzip(request.body))
  spans = payload.resource_spans.flat_map(&:scope_spans).flat_map(&:spans)
  lock.synchronize { exported.concat(spans) }
  { status: 200 }
end

# Configure before the suite, like a Rails initializer. The collector must
# attach to this provider without replacing its processor or instrumentation.
ENV["OTEL_TRACES_EXPORTER"] = "none"
customer = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
OpenTelemetry::SDK.configure do |config|
  config.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(customer))
  config.use "OpenTelemetry::Instrumentation::Rack"
  config.use "OpenTelemetry::Instrumentation::Net::HTTP"
end
provider = OpenTelemetry.tracer_provider
otel = Buildkite::TestCollector::OTel
tracer = provider.tracer("rack-fixture")
started = Queue.new
finish = Queue.new
server_threads = []
# Read after the app returns, while Rack::Events still holds the server context.
LazyBody = Struct.new(:tracer, :path) do
  def each
    tracer.in_span("body #{path}") { yield "ok" }
  end
end
app = lambda do |env|
  server_threads << Thread.current.object_id
  tracer.in_span("work #{env['PATH_INFO']}") do
    if env["PATH_INFO"] == "/late"
      started << true
      Timeout.timeout(10) { finish.pop }
    end
    [200, { "content-type" => "text/plain" }, LazyBody.new(tracer, env["PATH_INFO"])]
  end
end
middleware, *args = OpenTelemetry::Instrumentation::Rack::Instrumentation.instance.middleware_args
app = middleware.new(app, *args)
app = otel::RackMiddleware.new(app) unless ENV["WITHOUT_BRIDGE"]
Capybara.server = :webrick
server = Capybara::Server.new(app).boot
uri = URI("http://#{server.host}:#{server.port}")

# Suppress only client instrumentation so controls reach the server with
# exactly the supplied headers (including deliberately missing traceparent).
request = lambda do |path, headers = {}|
  OpenTelemetry::Common::Utilities.untraced do
    Net::HTTP.start(uri.host, uri.port) { |http| http.get(path, headers) }
  end
end

ENV["BUILDKITE_ANALYTICS_OTLP_ENDPOINT"] = endpoint
ENV["BUILDKITE_ANALYTICS_TOKEN"] = "fixture-token"
ENV["BUILDKITE_ANALYTICS_KEY"] = "rack-fixture"
Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)
headers = {}
late_request = nil
test_thread = Thread.current.object_id
RSpec.describe "in-process Rack requests" do
  after do |example|
    raise "intentional failure after the request started" if example.metadata[:fail_after]
  end

  it "sends traced HTTP and leaves one request in flight", :fail_after do
    response = Net::HTTP.start(uri.host, uri.port) { |http| http.get("/traced") }
    expect(response.body).to eq("ok")
    OpenTelemetry.propagation.inject(headers)
    headers["tracestate"] = "vendor=value"
    late_request = Thread.new { request.call("/late", headers) }
    Timeout.timeout(10) { started.pop }
  end

  it "does not claim stale, unknown, malformed, or headerless requests" do
    finish << true
    expect(late_request.value.body).to eq("ok")
    request.call("/stale", headers)
    request.call("/unknown", { "traceparent" => "00-#{'a' * 32}-#{'b' * 16}-01" })
    request.call("/malformed", { "traceparent" => "invalid" })
    request.call("/headerless")
    response = Net::HTTP.start(uri.host, uri.port) { |http| http.get("/second") }
    expect(response.body).to eq("ok")
  end

  it "propagates a phase directly without an instrumented HTTP client" do
    carrier = {}
    OpenTelemetry.propagation.inject(carrier)
    expect(request.call("/direct", carrier).body).to eq("ok")
  end
end
status = RSpec::Core::Runner.run(["--options", "/dev/null", "--format", "progress"], $stderr, $stderr)
otel.force_flush
raise "customer provider replaced" unless provider.equal?(OpenTelemetry.tracer_provider)
raise "requests did not use a server thread" if server_threads.include?(test_thread)

serialize = lambda do |span|
  {
    name: span.name, trace: span.trace_id.unpack1("H*"), id: span.span_id.unpack1("H*"),
    parent: span.parent_span_id.unpack1("H*"),
  }
end
puts JSON.generate(
  status: status,
  exported: exported.map do |span|
    serialize.call(span).merge(
      attributes: span.attributes.to_h { |kv| [kv.key, kv.value.string_value] },
      tracestate: span.trace_state,
      kind: span.kind.to_s,
    )
  end,
  customer: customer.finished_spans.map(&serialize),
)
otel.shutdown
exit status
