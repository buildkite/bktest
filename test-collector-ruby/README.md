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

### OpenTelemetry export (experimental)

RSpec suites can also send an OpenTelemetry trace per test execution to Buildkite,
showing what each test did and where it spent its time. Each trace is rooted in a
`test.execution` span carrying its name, location, result, and any failure detail.
Tags passed to `configure` appear as resource attributes, while `tag_execution`
adds attributes to the current test's root span.

This feature is still under development and may change. This first release is
intended for suites that do not already configure OpenTelemetry. It may work
with an existing OpenTelemetry setup, but that configuration is not yet
supported or guaranteed to work.

OpenTelemetry export is off by default. Opt in when you configure the collector:

```ruby
Buildkite::TestCollector.configure(hook: :rspec, otel_enabled: true)
```

Execution roots use a private AlwaysOn provider so a suite's sampling policy
cannot remove them.

The collector configures a global provider for child spans and installs all
applicable instrumentation registered when the suite starts. Add the
OpenTelemetry SDK and OTLP exporter, plus any instrumentation you want to use:

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

Export needs Ruby 3.3 or newer, which is what the OpenTelemetry gems require. If
those gems are unavailable, the option is accepted and export remains disabled.

The collector honors standard `OTEL_EXPORTER_OTLP_TRACES_HEADERS` (or the
generic `OTEL_EXPORTER_OTLP_HEADERS`) and gives them precedence over its own
headers, including `Authorization`. bktec's OTLP relay uses this to provide its
local credential without changing `BUILDKITE_ANALYTICS_TOKEN`, which remains
available for normal JSON uploads in `otel_enabled` mode. Without an OTLP
Authorization header, spans use `BUILDKITE_ANALYTICS_TOKEN`, which must be an
agent OIDC token with the `write_uploads` scope; a suite API token still uploads
executions, but its spans are rejected.

Export failures never fail a test or block the normal Test Engine upload. See the
[OpenTelemetry guide](docs/opentelemetry.md) for setup details and current
limitations.

### OTLP-only submission (experimental)

RSpec suites can go one step further and submit results *only* over OTLP, with
no JSON upload at all. It exports the same spans as `otel_enabled` and adds
`buildkite.execution.via=otlp`, which tells Buildkite to synthesize each test
execution from its span server-side:

```ruby
Buildkite::TestCollector.configure(hook: :rspec, otel_only: true)
```

In this mode the collector's legacy machinery is switched off: nothing is
uploaded to `/v1/uploads`, and `Net::HTTP` and `Object` are left unpatched. The
gem's whole job is to configure OpenTelemetry so each test gets a suitable span:

- `Buildkite::TestCollector.annotate` adds a `test.annotation` event to the
  current span.
- `Buildkite::TestCollector.tag_execution` sets attributes on the test span.
- `tags:` given to `configure` become resource attributes on every span.
- Instrumentation works exactly as it does with `otel_enabled`: everything you
  require and register installs, and `otel_instrumentations: []` exports only
  the `test.execution` spans. See
  [choosing instrumentation](docs/opentelemetry.md#choosing-instrumentation).
- Your code can also talk to OpenTelemetry directly — the collector configures
  the global tracer provider, so
  `OpenTelemetry::Trace.current_span.set_attribute(...)` works during a test,
  and any instrumentation joins the test's trace.

`otel_only` is currently RSpec-only and needs Ruby 3.3+. It's an alternative to
`otel_enabled`; the two are mutually exclusive, and passing both (either value)
raises `ArgumentError`.

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
