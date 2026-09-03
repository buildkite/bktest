# Buildkite Collectors for Ruby

**DEPRECATION NOTICE**
Versions prior to 2.1.x are unsupported and will not work after mid-2023. Please upgrade to the latest version.

Official [Buildkite Test Engine](https://buildkite.com/platform/test-engine) collectors for Ruby test frameworks ✨

⚒ **Supported test frameworks:** RSpec, Minitest, Cucumber, and [more coming soon](https://github.com/buildkite/test-collector-ruby/issues?q=is%3Aissue+is%3Aopen+label%3A%22test+frameworks%22).

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, Codeship, and others via the `BUILDKITE_ANALYTICS_*` environment variables.

## 👉 Installing

### Step 1

[Create a test suite](https://buildkite.com/docs/test-analytics), and copy the API token that it gives you.

Add the [`buildkite-test_collector`](https://rubygems.org/gems/buildkite-test_collector) gem:

```shell
gem install buildkite-test_collector
```

Or add this to your Gemfile’s test group:

```ruby
group :test do
  gem 'buildkite-test_collector'
end
```

### Step 2

#### RSpec

Add the following code to your RSpec setup file:

```ruby
# spec/spec_helper.rb
require 'buildkite/test_collector'
Buildkite::TestCollector.configure(hook: :rspec)
```

Run your tests locally:

```shell
BUILDKITE_ANALYTICS_TOKEN=xyz rspec
```

#### Minitest

Add the following code to your Minitest setup file:

```ruby
# test/test_helper.rb
require 'buildkite/test_collector'
Buildkite::TestCollector.configure(hook: :minitest)
```

Run your tests locally:

```shell
BUILDKITE_ANALYTICS_TOKEN=xyz rake
```

#### Cucumber

Add the following code to your Cucumber setup file:

```ruby
# features/support/env.rb
require 'buildkite/test_collector'
Buildkite::TestCollector.configure(hook: :cucumber)
```

Run your tests locally:

```shell
BUILDKITE_ANALYTICS_TOKEN=xyz cucumber
```

### Step 3

Add the `BUILDKITE_ANALYTICS_TOKEN` secret to your CI, push your changes to a branch, and open a pull request 🎉

### OpenTelemetry submission (experimental)

RSpec suites can submit each test execution as an OpenTelemetry trace. Each
trace is rooted in a `test.execution` span carrying the test's name, location,
result, and failure detail. Instrumented child spans show what the test did and
where it spent its time.

This feature is still under development and may change. OpenTelemetry is off by
default. Enable it when you configure the collector:

```ruby
Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)
```

When enabled, OpenTelemetry is the only submission path. Every root includes
`buildkite.execution.via=otlp`, which tells Buildkite to synthesize the test
execution from the span. Nothing is uploaded to `/v1/uploads`, and the legacy
`Net::HTTP` and `Object` tracing patches are not installed.

Execution roots use a private AlwaysOn provider so a suite's sampling policy
cannot remove them. The collector also configures a global provider for child
spans and installs all applicable instrumentation registered when the suite
starts. Because the OpenTelemetry dependencies require Ruby 3.3+, the collector
does not install them automatically:

```ruby
# Gemfile
gem "opentelemetry-exporter-otlp", "~> 0.34", require: false
gem "opentelemetry-sdk", "~> 1.13", require: false
gem "opentelemetry-instrumentation-pg", require: false

# spec/spec_helper.rb
require "opentelemetry-instrumentation-pg"
require "buildkite/test_collector"

Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)
```

Adding a gem to the Gemfile may auto-require it in applications that call
`Bundler.require`, but that is not guaranteed. An explicit `require` is the
recommended setup. To disable instrumentations and export only root
`test.execution` spans, set `otel_instrumentations: []`. Any other value is
reserved for a future release and disables span export with a warning. See the
[OpenTelemetry guide](docs/opentelemetry.md#choosing-instrumentation) for more.

The collector honors standard `OTEL_EXPORTER_OTLP_TRACES_HEADERS` (or the
generic `OTEL_EXPORTER_OTLP_HEADERS`) and gives them precedence over its own
headers, including `Authorization`. Without an OTLP Authorization header, spans
use `BUILDKITE_ANALYTICS_TOKEN`, which must be an agent OIDC token with the
`write_uploads` scope; a suite API token is rejected by the OTLP endpoint.

If OpenTelemetry cannot be configured, or Buildkite rejects the exported
`test.execution` spans, the collector prints a prominent warning. Tests continue
to run, but there is no JSON fallback and the affected results are not uploaded.
See the [OpenTelemetry guide](docs/opentelemetry.md) for setup details and
current limitations.

## More information

For more use cases such as custom tags, annotations, and span tracking, please visit our [official Ruby collector documentation](https://buildkite.com/docs/test-engine/ruby-collectors) for details.

## ⚒ Developing

After cloning the repository, install the dependencies:

```
bundle
```

And run the tests:

```
bundle exec rspec
```

Useful resources for developing collectors include the [Buildkite Test Engine docs](https://buildkite.com/docs/test-engine).

See [DESIGN.md](DESIGN.md) for an overview of the design of this gem.

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/buildkite/test-collector-ruby

## 🚀 Releasing

See the monorepo's [collector release guide](../RELEASING.md#ruby-rubygems).

## 📜 MIT License

The gem is available as open source under the terms of the [MIT License](https://opensource.org/licenses/MIT).
