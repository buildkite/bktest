# Buildkite Test Collector for Python

The official Python adapter for [Buildkite Test Engine](https://buildkite.com/platform/test-engine/) which collects information about your tests.

**Supported Python versions:** >=3.10

⚒ **Supported test frameworks:** pytest.

📦 **Supported CI systems:** Buildkite, GitHub Actions, CircleCI, and others via the `BUILDKITE_ANALYTICS_*` environment variables.


## 👉 Installing

1. [Create a test suite](https://buildkite.com/docs/test-engine), and copy the API token that it gives you.

2. Add `buildkite-test-collector` to your project dependencies

Using uv:
```sh
uv add --dev buildkite-test-collector
```

Or add it to your `pyproject.toml`:
```toml
[project.optional-dependencies]
dev = [
    "buildkite-test-collector"
]
```

3. Set up your API token

Add the `BUILDKITE_ANALYTICS_TOKEN` environment variable to your build system's environment.

4. Run your tests

Run your tests like normal. Note that we attempt to detect the presence of several common CI environments, however if this fails you can set the `CI` environment variable to any value and it will work.

```sh
uv run pytest
```

5. Verify that it works

If all is well, you should see the test run in the Test Engine section of the Buildkite dashboard.

## 🏷️ Filtering Tests by Tags

You can filter which tests to run based on execution tags using the `--tag-filters` option.

First, mark your tests with execution tags:

```python
import pytest

@pytest.mark.execution_tag("color", "red")
def test_red_feature():
    assert True

@pytest.mark.execution_tag("color", "blue")
def test_blue_feature():
    assert True
```

Then filter tests by tag using the `--tag-filters` option with `key:value` format:

```sh
# Run only tests tagged with color:red
pytest --tag-filters "color:red"

# Run only tests tagged with color:blue
pytest --tag-filters "color:blue"
```

**Note:** The `--tag-filters` option performs exact key:value matching. Only tests with the specified tag will be selected.

## 🔁 Running batches with `bktec pool exec`

The package includes `buildkite-pytest`, a persistent runner for
[`bktec pool exec`](https://github.com/buildkite/test-engine-client). It
starts pytest once and runs every batch bktec assigns in the same pytest
process. Each test file is imported and collected the first time a batch needs
it, and session-scoped fixtures are set up once per job. Module- and
class-scoped fixtures are torn down after each batch.

pytest support in `bktec pool exec` requires bktec v3.4.0 or later (a
pre-release at the time of writing); bktec v3.3.0 rejects it with
`pool planning requires rspec`.

```sh
BUILDKITE_TEST_ENGINE_TEST_RUNNER=pytest bktec pool exec -- buildkite-pytest [pytest options]
```

bktec chooses which tests run and retries failures, so `buildkite-pytest`
rejects test arguments and options that select, retry or stop tests, such as
`-k`, `-m`, `-x`, `--maxfail`, `--lf`, `--sw`, `-n` (pytest-xdist),
`--reruns` (pytest-rerunfailures), `--json` and `--tag-filters`. Run it from
pytest's rootdir. Each batch's test executions are uploaded to Test Engine
using `BUILDKITE_ANALYTICS_TOKEN`.

## 🎢 Tracing

Buildkite Test Engine has support for tracing potentially slow operations within your tests, and can collect span data of [four types](https://buildkite.com/docs/test-engine/importing-json#json-test-results-data-reference-span-objects): http, sql, sleep and annotations. This is documented as part of our public JSON API so anyone can instrument any code to send this data.

This library supports the ability to transmit tracing information to your Test Engine output by using the new `spans` pytest fixture.  See [the `SpanCollector` documentation](https://github.com/buildkite/test-collector-python/blob/main/src/buildkite_test_collector/pytest_plugin/span_collector.py) for more information.

You may also need to manually capture the data you wish to trace for your use case. For examples of how we've done this in our Ruby test collector, see:
- [Instrumenting Rails to capture sql data](https://github.com/buildkite/test-collector-ruby/blob/9ac2b465cad647790d89b501a1754b06e47d5997/lib/buildkite/test_collector.rb#L107)
- [Monkey patching to various libraries to capture http requests](https://github.com/buildkite/test-collector-ruby/blob/9ac2b465cad647790d89b501a1754b06e47d5997/lib/buildkite/test_collector/network.rb#L58)
- [Monkey patching for sleep](https://github.com/buildkite/test-collector-ruby/blob/9ac2b465cad647790d89b501a1754b06e47d5997/lib/buildkite/test_collector/object.rb#L20)

Note: the Ruby test collector is the only Test Engine collector that automatically captures and transmits span data. This Python collector can transmit information, but data capture must be done manually at this time.

## 🌏 Environment variables

| Variable | Purpose |
| --- | --- |
| `BUILDKITE_ANALYTICS_TOKEN` | Suite API token. Required for the JSON upload; also the OpenTelemetry credential when `BUILDKITE_TESTS_OTLP_TOKEN` is not set. |
| `BUILDKITE_ANALYTICS_API_URL` | Base URL of the JSON upload API. Default `https://analytics-api.buildkite.com/v1`. |
| `BUILDKITE_ANALYTICS_OTEL_ENABLED` | `true` (or `1`) sends results over OpenTelemetry instead; see below. |
| `BUILDKITE_ANALYTICS_DEBUG_ENABLED` | `1` logs the collector's hooks and uploads. |

The run's metadata (key, URL, branch, commit, number, job ID, message) is detected from Buildkite, GitHub Actions or CircleCI. A non-empty value of any of these overrides the detected one, on both the JSON and the OpenTelemetry path:

| Variable | Run field |
| --- | --- |
| `BUILDKITE_ANALYTICS_KEY` | Run key; executions from processes sharing a key land in one run. Default: the Buildkite build ID, the GitHub Actions or CircleCI run identifier, or a random UUID per process outside CI. |
| `BUILDKITE_ANALYTICS_URL` | Link back to the build. |
| `BUILDKITE_ANALYTICS_BRANCH` | Branch. |
| `BUILDKITE_ANALYTICS_SHA` | Commit SHA. |
| `BUILDKITE_ANALYTICS_NUMBER` | Build number. |
| `BUILDKITE_ANALYTICS_JOB_ID` | Job ID. |
| `BUILDKITE_ANALYTICS_MESSAGE` | Commit message. |

A JSON upload with a custom `BUILDKITE_ANALYTICS_KEY` is not linked to its Buildkite build in Test Engine.

## 🔄 Changes to the JSON upload in 1.10.0

These apply whether or not OpenTelemetry submission is on:

- A test whose fixture teardown raises is uploaded as failed, with the teardown error as the reason. It used to be uploaded with the result from before teardown (passed or skipped), except under pytest-forked, which already uploaded it as failed. If the test had already failed, that failure now stays the reason and the teardown error is added as a further `failure_expanded` entry; under pytest-forked the teardown error used to replace the reason.
- With pytest-rerunfailures (`--reruns`), each attempt is uploaded as its own execution: an attempt pytest reports as `rerun` is failed, and the last attempt has its own result. Previously no result was uploaded for any test in a `--reruns` run, with a `has no result set` warning.
- Test locations use 1-based line numbers (`tests/test_x.py:1` for a test on the first line). They were one line early. Test identity is unchanged.
- The `BUILDKITE_ANALYTICS_KEY`, `_URL`, `_BRANCH`, `_SHA`, `_NUMBER`, `_JOB_ID` and `_MESSAGE` overrides in the table above now take effect, as they do in the Ruby and JavaScript collectors.

## 🔭 OpenTelemetry submission (experimental)

The collector can send each test to Test Engine as an OpenTelemetry `test.execution` span over OTLP instead of the JSON upload, with your tests' own OpenTelemetry spans nested under it. Install the optional dependencies and switch it on:

```sh
pip install "buildkite-test-collector[otel]"
BUILDKITE_ANALYTICS_OTEL_ENABLED=true pytest
```

See [docs/opentelemetry.md](docs/opentelemetry.md) for what is sent, how child spans and the span filter work, and what the warnings mean.

## 🔜 Roadmap

See the [GitHub 'enhancement' issues](https://github.com/buildkite/test-collector-python/issues?q=is%3Aissue+is%3Aopen+label%3Aenhancement) for planned features. Pull requests are always welcome, and we’ll give you feedback and guidance if you choose to contribute 💚

## ⚒ Developing

After cloning the repository, install [uv](https://docs.astral.sh/uv/) if you haven't already:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

Then install the dependencies:

```bash
uv sync --all-extras
```

And run the tests:

```bash
uv run pytest
```

Useful resources for developing collectors include the [Buildkite Test Engine docs](https://buildkite.com/docs/test-engine) and the [RSpec and Minitest collectors](https://github.com/buildkite/test-collector-ruby).

## 👩‍💻 Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/buildkite/test-collector-python

## 🚀 Releasing

See the monorepo's [collector release guide](../RELEASING.md#python-pypi).

## 📜 License

The package is available as open source under the terms of the [MIT License](https://opensource.org/licenses/MIT).

## 🤙 Thanks

Thanks to the folks at [Alembic](https://alembic.com.au/) for building and maintaining this package.
