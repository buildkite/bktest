"""OTel mode switches, credentials, run keys and processor size overrides."""

import pytest

from buildkite_test_collector.pytest_plugin.otel import config
from buildkite_test_collector.pytest_plugin.otel import plugin as otel_plugin


def test_otel_is_off_unless_opted_in(fake_env):
    env = {"BUILDKITE_TESTS_OTLP_TOKEN": "t"}
    assert otel_plugin.build(env, fake_env, {}, collector=None) is None
    assert config.OTelConfig.from_env({**env, "BUILDKITE_ANALYTICS_OTEL_ENABLED": "yes"}).enabled is False
    assert config.OTelConfig.from_env({**env, "BUILDKITE_ANALYTICS_OTEL_ENABLED": "TRUE"}).enabled is True
    assert config.OTelConfig.from_env({**env, "BUILDKITE_ANALYTICS_OTEL_ENABLED": "1"}).enabled is True


class RecordingEnv(dict):
    """An environment that remembers which variables were read."""

    def __init__(self, values):
        super().__init__(values)
        self.read = set()

    def get(self, key, default=None):
        self.read.add(key)
        return super().get(key, default)

    def __getitem__(self, key):
        self.read.add(key)
        return super().__getitem__(key)

    def __contains__(self, key):
        self.read.add(key)
        return super().__contains__(key)


@pytest.mark.parametrize("switch", [None, "false"])
def test_otel_off_reads_nothing_but_the_switch(fake_env, switch):
    env = RecordingEnv({
        "BUILDKITE_TESTS_OTLP_TOKEN": "t",
        "BUILDKITE_ANALYTICS_TOKEN": "json",
        "BUILDKITE_ANALYTICS_OTLP_ENDPOINT": "http://localhost:4318/v1/traces",
    })
    if switch is not None:
        env["BUILDKITE_ANALYTICS_OTEL_ENABLED"] = switch
    assert otel_plugin.build(env, fake_env, {}, collector=None) is None
    assert env.read == {"BUILDKITE_ANALYTICS_OTEL_ENABLED"}


def test_otel_is_off_without_a_credential_like_the_json_path(fake_env, capfd):
    env = {"BUILDKITE_ANALYTICS_OTEL_ENABLED": "true", "BUILDKITE_TESTS_OTLP_TOKEN": "  "}
    assert otel_plugin.build(env, fake_env, {}, collector=None) is None
    assert capfd.readouterr().err == ""


def test_relay_token_is_preferred_and_blank_means_unset():
    on = {"BUILDKITE_ANALYTICS_OTEL_ENABLED": "true"}
    both = {**on, "BUILDKITE_TESTS_OTLP_TOKEN": " relay ", "BUILDKITE_ANALYTICS_TOKEN": "json"}
    assert config.OTelConfig.from_env(both).token == "relay"
    blank_relay = {**on, "BUILDKITE_TESTS_OTLP_TOKEN": "   ", "BUILDKITE_ANALYTICS_TOKEN": "json"}
    assert config.OTelConfig.from_env(blank_relay).token == "json"


def test_endpoint_defaults_and_can_be_overridden():
    assert config.OTelConfig.from_env({}).endpoint == "https://tests-otlp.buildkite.com/v1/traces"
    custom = {
        "BUILDKITE_ANALYTICS_OTEL_ENABLED": "true",
        "BUILDKITE_ANALYTICS_OTLP_ENDPOINT": "http://localhost:4318/v1/traces",
    }
    assert config.OTelConfig.from_env(custom).endpoint == "http://localhost:4318/v1/traces"


def test_fallback_outcome_depends_on_where_the_credential_came_from():
    assert config.json_fallback_outcome({"BUILDKITE_ANALYTICS_TOKEN": "json"}) \
        == "uploading results as JSON instead"
    assert config.json_fallback_outcome({"BUILDKITE_TESTS_OTLP_TOKEN": "relay"}) \
        == "results will not be uploaded because BUILDKITE_ANALYTICS_TOKEN is not set"


@pytest.mark.parametrize("run_key, valid", [
    ("a", True),
    ("a" * 255, True),
    ("a" * 256, False),
    ("", False),
    ("!#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", True),
    ("has space", False),
    ("tab\there", False),
    ("ünïcode", False),
])
def test_run_key_validation(run_key, valid):
    assert config.run_key_is_valid(run_key) is valid


def test_span_processor_sizes_accept_overrides_up_to_the_limit(capfd):
    env = {
        config.TEST_SPAN_QUEUE_SIZE_ENV: str(2**31 - 1),
        config.TEST_SPAN_BATCH_SIZE_ENV: "007",
    }
    sizes = config.span_processor_sizes(
        env, config.TEST_SPAN_QUEUE_SIZE_ENV, config.TEST_SPAN_BATCH_SIZE_ENV,
        config.TEST_SPAN_DEFAULT_SIZES,
    )
    assert sizes == (2**31 - 1, 7)
    assert capfd.readouterr().err == ""


@pytest.mark.parametrize("raw", ["0", "-1", "abc", "", str(2**31), "1.5", " 7", "+7", "9" * 5000])
def test_span_processor_sizes_warn_and_use_the_default_for_invalid_values(raw, capfd):
    env = {config.TEST_SPAN_BATCH_SIZE_ENV: raw}
    sizes = config.span_processor_sizes(
        env, config.TEST_SPAN_QUEUE_SIZE_ENV, config.TEST_SPAN_BATCH_SIZE_ENV,
        config.TEST_SPAN_DEFAULT_SIZES,
    )
    assert sizes == config.TEST_SPAN_DEFAULT_SIZES
    err = capfd.readouterr().err
    assert err.count("WARNING") == 1
    assert "BUILDKITE_TESTS_OTEL_TEST_SPAN_BATCH_SIZE must be a positive integer" in err


def test_batch_larger_than_queue_resets_both_to_defaults(capfd):
    env = {config.CHILD_SPAN_QUEUE_SIZE_ENV: "10", config.CHILD_SPAN_BATCH_SIZE_ENV: "11"}
    sizes = config.span_processor_sizes(
        env, config.CHILD_SPAN_QUEUE_SIZE_ENV, config.CHILD_SPAN_BATCH_SIZE_ENV,
        config.CHILD_SPAN_DEFAULT_SIZES,
    )
    assert sizes == config.CHILD_SPAN_DEFAULT_SIZES
    err = capfd.readouterr().err
    assert err.count("WARNING") == 1
    assert "BUILDKITE_TESTS_OTEL_CHILD_SPAN_BATCH_SIZE must be <= " \
        "BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE" in err


def test_child_sizes_do_not_affect_test_sizes():
    env = {config.CHILD_SPAN_QUEUE_SIZE_ENV: "3", config.CHILD_SPAN_BATCH_SIZE_ENV: "3"}
    test_sizes = config.span_processor_sizes(
        env, config.TEST_SPAN_QUEUE_SIZE_ENV, config.TEST_SPAN_BATCH_SIZE_ENV,
        config.TEST_SPAN_DEFAULT_SIZES,
    )
    child_sizes = config.span_processor_sizes(
        env, config.CHILD_SPAN_QUEUE_SIZE_ENV, config.CHILD_SPAN_BATCH_SIZE_ENV,
        config.CHILD_SPAN_DEFAULT_SIZES,
    )
    assert test_sizes == config.TEST_SPAN_DEFAULT_SIZES
    assert child_sizes == (3, 3)
