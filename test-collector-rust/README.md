# Buildkite Test Collector for Rust (Beta)

The official Rust adapter for [Buildkite Test Analytics](https://buildkite.com/test-analytics) which implements a parser and sender for Rust's JSON test output.

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, and others via the `BUILDKITE_ANALYTICS_*` environment variables.

## 👉 Installing

1. [Create a test suite](https://buildkite.com/docs/test-analytics), and copy the API token that it gives you.

2. Install the `buildkite-test-collector` crate

```sh
cargo install buildkite-test-collector
```

Alternatively you can install direct from the repo

```sh
cargo install --git https://github.com/buildkite/test-collector-rust buildkite-test-collector
```

3. Configure your environment

Set the `BUILDKITE_ANALYTICS_TOKEN` environment variable to contain the
token provided by the analytics project settings.

We try and detect several common CI environments based in the environment
variables which are present. If this detection fails then the application will
crash with an error. To force the use of a "generic CI environment" just set
the `CI` environment variable to any non-empty value.

3. Change your test output to JSON format

In your CI environment you will need to change your output format to `JSON` and
add `--report-time` to include execution times in the output. Unfortunately,
these are currently unstable options for Rust, so some extra command line
options are needed. Once you have the JSON output you can simply pipe it
through the `buildkite-test-collector` binary - the input JSON is echoed back
to STDOUT so that you can still operate upon it if needed.

```sh
cargo test -- -Z unstable-options --format json --report-time | buildkite-test-collector
```

4. Confirm correct operation

Verify that the run is visible in the Buildkite analytics dashboard

## 🔜 Roadmap

See the [GitHub 'enhancement' issues](https://github.com/buildkite/test-collector-rust/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement) for planned features. Pull requests are always welcome, and we’ll give you feedback and guidance if you choose to contribute 💚

## ⚒ Developing

After cloning the repository, run the tests:

```
cargo test
```

Useful resources for developing collectors include the [Buildkite Test Analytics docs](https://buildkite.com/docs/test-analytics) and the [RSpec and Minitest collectors](https://github.com/buildkite/rspec-buildkite-analytics).

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/buildkite/test-collector-rust

## 🚀 Releasing

See the monorepo's [collector release guide](../RELEASING.md#rust-cratesio).

## 📜 License

The package is available as open source under the terms of the [MIT License](https://opensource.org/licenses/MIT).

## 🤙 Thanks

Thanks to the folks at [Alembic](https://alembic.com.au/) for building and maintaining this package.
