# Buildkite Test Collector for .NET (Beta)

The official .NET adapter for [Buildkite Test Analytics](https://buildkite.com/test-analytics) which collects information about your tests.

This package contains the API integrations and data structures requried to interact with the Buildkite Test Analytics API.  You normally wouldn't use it directly, rather via a specific test runner integration.

## ⚒ Developing

1. Cloning the repository.
2. Run the tests:
   `dotnet test`

Useful resources for developing collectors include the [Buildkite Test Analytics docs](https://buildkite.com/docs/test-analytics) and the [RSpec and Minitest collectors](https://github.com/buildkite/rspec-buildkite-analytics).

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/buildkite/test-collector-dotnet

## 🚀 Releasing

See the monorepo's [collector release guide](../../RELEASING.md#net-nuget).

## 📜 License

The package is available as open source under the terms of the [MIT License](https://opensource.org/licenses/MIT).

## 🤙 Thanks

Thanks to the folks at [Alembic](https://alembic.com.au/) for building and maintaining this package.
