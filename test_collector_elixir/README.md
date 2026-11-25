# Buildkite Test Collector for Elixir (Beta)

The official Elixir adapter for [Buildkite Test Engine](https://buildkite.com/platform/test-engine), which uses an ExUnit formatter to collect information about your tests.

**Supported Elixir versions:** `~> 1.16`

⚒ **Supported test frameworks:** ExUnit.

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, generic CI via the `CI` environment variable, and local runs via `BUILDKITE_TEST_ANALYTICS_LOCAL`.

## 👉 Installing

1. [Create a test suite](https://buildkite.com/docs/test-engine), and copy the API token that it gives you.

2. Add [`buildkite_test_collector`](https://hex.pm/packages/buildkite_test_collector) to your list of dependencies in `mix.exs`:

   ```elixir
   def deps do
     [
       {:buildkite_test_collector, "~> 0.3.2", only: [:test]}
     ]
   end
   ```

3. Set up your API token.

   In your `config/test.exs` (or other environment configuration as appropriate), configure `:api_key` with your test suite API token. We suggest retrieving the token from the environment and configuring your CI environment accordingly (e.g. via secrets):

   ```elixir
   import Config

   config :buildkite_test_collector,
     api_key: System.get_env("BUILDKITE_ANALYTICS_TOKEN")
   ```

   Optional application configuration keys:
   - `:batch_size` — number of test results to buffer before uploading a batch (default: `100`).
   - `:api_endpoint` — upload endpoint URL (default: `"https://analytics-api.buildkite.com/v1/uploads"`).

4. Add `BuildkiteTestCollector.Formatter` to your ExUnit configuration in `test/test_helper.exs`:

   ```elixir
   ExUnit.configure(formatters: [BuildkiteTestCollector.Formatter, ExUnit.CLIFormatter])
   ExUnit.start()
   ```

5. Run your tests.

   Run your tests like normal. The formatter automatically detects Buildkite (`BUILDKITE_BUILD_ID`), CircleCI (`CIRCLE_BUILD_NUM`), and GitHub Actions (`GITHUB_RUN_NUMBER`). For other CI environments, set `CI` to any value; to send results from a local run, set `BUILDKITE_TEST_ANALYTICS_LOCAL` to any value. When none of these variables are set, the formatter logs a warning and does not upload results.

   ```sh
   $ mix test

   ...

   Finished in 0.01 seconds (0.003s on load, 0.004s on tests)
   3 tests, 0 failures

   Randomized with seed 12345
   ```

6. Verify that it works.

   If all is well, you should see the test run in your Test Engine suite in Buildkite.

## 🎢 Tracing

Buildkite Test Engine has support for tracing potentially slow operations within your tests (SQL queries, HTTP requests, etc.). Because ExUnit can run multiple tests simultaneously, it is difficult to achieve this without requiring code changes—we cannot simply use [telemetry](https://hex.pm/packages/telemetry) events because we cannot easily attribute the events to specific tests across process boundaries. Instead, [`BuildkiteTestCollector.Tracing`](https://hexdocs.pm/buildkite_test_collector/BuildkiteTestCollector.Tracing.html) provides `measure/3` and `measure/4` (supporting `:http`, `:sql`, `:sleep`, and `:annotation` spans) to manually instrument operations within your tests.

To record an annotation, pass `%{content: "message"}` as the detail:

```elixir
alias BuildkiteTestCollector.Tracing

test "it records an annotation", tags do
  assert :ok =
           Tracing.measure(tags, :annotation, %{content: "checkpoint"}, fn ->
             :ok
           end)
end
```

The function's return value and the detail are passed through unchanged. Annotation
messages must be in `detail.content`, as required by the [JSON span schema](https://buildkite.com/docs/test-engine/importing-json#json-test-results-data-reference-span-objects).
A plain string detail does not populate that field. The same detail map applies
when adding an annotation directly with `BuildkiteTestCollector.Formatter.add_span/2`.

## 🔜 Roadmap

See the [GitHub 'enhancement' issues](https://github.com/buildkite/bktest/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement) for planned features. Pull requests are always welcome, and we’ll give you feedback and guidance if you choose to contribute 💚

## ⚒ Developing

After cloning the repository, change into the Elixir collector directory and install the dependencies:

```sh
cd test_collector_elixir
mix deps.get
```

And run the tests and formatter check:

```sh
mix test
mix format --check-formatted
```

Useful resources for developing collectors include the [Buildkite Test Engine docs](https://buildkite.com/docs/test-engine) and the [Ruby collectors](https://github.com/buildkite/bktest/tree/main/test-collector-ruby).

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/buildkite/bktest.

Please use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
The version and changelog are maintained explicitly in a release pull request.

## 🚀 Releasing

See the monorepo's [collector release guide](../RELEASING.md#elixir-hex).

## 📜 License

The package is available as open source under the terms of the [MIT License](https://opensource.org/licenses/MIT).
