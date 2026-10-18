// Does an end-to-end test of the Playwright example, using the debug output from the
// reporter, and verifying the JSON
require('dotenv').config();
const { exec, execFile, spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const os = require('os');
const { startOtlpServer } = require('./support/otlpServer');

const TIMEOUT = 20000;

const cwd = path.join(__dirname, "../examples/playwright");
const runPlaywright = (args, env) => {

  return new Promise((resolve) => {
    const command = `npm test -- ${args.join(' ')}`
    exec(command, { cwd, env: { ...env, JEST_WORKER_ID: undefined } }, (error, stdout) => {
      resolve(stdout)
    })
  })
}

const expectOutputToHaveToken = (stdout, expectation) => {
  const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
  const headers = JSON.parse(jsonMatch[1])["headers"]

  expect(headers).toHaveProperty("Authorization", `Token token="${expectation}"`)
}

describe('examples/playwright', () => {
  const env = {
    ...process.env,
    BUILDKITE_ANALYTICS_TOKEN: "xyz",
    BUILDKITE_ANALYTICS_DEBUG_ENABLED: "true"
  }

  describe('when token is defined through reporter options', () => {
    test('it uses the correct token', async () => {
      const stdout = await runPlaywright(["--config", "token.config.js"], { ...env, BUILDKITE_ANALYTICS_PLAYWRIGHT_TOKEN: 'abc' })

      expectOutputToHaveToken(stdout, 'abc')
    }, TIMEOUT)
  });

  describe('when token is defined through BUILDKITE_ANALYTICS_TOKEN', () => {
    test('it uses the correct token', async () => {
      const stdout = await runPlaywright([], env)

      expectOutputToHaveToken(stdout, 'xyz')
    }, TIMEOUT)
  });

  test('it posts the correct JSON', async () => {
    const stdout = await runPlaywright([], env)

    const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
    const data = JSON.parse(jsonMatch[1])["data"]

    expect(data).toHaveProperty("format", "json")

    expect(data).toHaveProperty("run_env.ci")
    expect(data).toHaveProperty("run_env.debug", 'true')
    expect(data).toHaveProperty("run_env.key")
    expect(data).toHaveProperty("run_env.version")
    expect(data).toHaveProperty("run_env.collector", "js-buildkite-test-collector")
    expect(data).toHaveProperty("run_env.test_runner", "playwright")

    expect(data).toHaveProperty("data[0].scope", ' chromium example.spec.js has title')
    expect(data).toHaveProperty("data[0].name", 'has title')
    expect(data).toHaveProperty("data[0].location", "tests/example.spec.js:3:1")
    expect(data).toHaveProperty("data[0].file_name", "tests/example.spec.js")
    expect(data).toHaveProperty("data[0].result", "passed")

    expect(data).toHaveProperty("data[1].scope", " chromium example.spec.js says hello")
    expect(data).toHaveProperty("data[1].name", "says hello")
    expect(data).toHaveProperty("data[1].location", "tests/example.spec.js:9:1")
    expect(data).toHaveProperty("data[1].file_name", "tests/example.spec.js")
    expect(data).toHaveProperty("data[1].result", "failed")
    expect(data).toHaveProperty("data[1].failure_reason", expect.stringContaining("expect(locator).toHaveText(expected)"))
    expect(data).toHaveProperty("data[1].failure_expanded", expect.arrayContaining([
      expect.objectContaining({
        expanded: expect.arrayContaining([expect.stringContaining('"Hello, World!"')])
      })
    ]))

    expect(data).toHaveProperty("data[2].tags", { foo: "bar" });
    expect(data).toHaveProperty("data[3].tags", { foo: "bar", baz: 'qux' });
    expect(data).toHaveProperty("data[4].tags", {});

    expect(stdout).toMatch(/Test Engine .* response/m)
  }, TIMEOUT);

  describe('when --retries option is used', () => {
    test("it posts all retried executions", async () => {
      const stdout = await runPlaywright(["--retries=1"], env)

      const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
      const data = JSON.parse(jsonMatch[1])["data"]["data"];

      const retriedTest = data.filter(test => test.name === "says hello")
      expect(retriedTest.length).toEqual(2)
      expect(retriedTest.map(test => test.result)).toEqual(["failed", "passed"])
      expect(stdout).toMatch(/Test Engine .* response/m)
    }, TIMEOUT)
  })

  describe('when --timeout is exceeded', () => {
    test("it posts the failures", async () => {
      const stdout = await runPlaywright(["--timeout=1"], env)

      const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
      const data = JSON.parse(jsonMatch[1])["data"];

      expect(data).toHaveProperty("format", "json")

      expect(data).toHaveProperty("run_env.ci")
      expect(data).toHaveProperty("run_env.debug", 'true')
      expect(data).toHaveProperty("run_env.key")
      expect(data).toHaveProperty("run_env.version")
      expect(data).toHaveProperty("run_env.collector", "js-buildkite-test-collector")
    expect(data).toHaveProperty("run_env.test_runner", "playwright")

      expect(data).toHaveProperty("data[0].scope", ' chromium example.spec.js has title')
      expect(data).toHaveProperty("data[0].name", 'has title')
      expect(data).toHaveProperty("data[0].location", "tests/example.spec.js:3:1")
      expect(data).toHaveProperty("data[0].file_name", "tests/example.spec.js")
      expect(data).toHaveProperty("data[0].result", 'failed')
      expect(data).toHaveProperty("data[1].failure_reason", expect.stringContaining("Test timeout of 1ms exceeded while setting up"))
      expect(data).toHaveProperty("data[1].failure_expanded", expect.arrayContaining([
        expect.objectContaining({
          expanded: expect.arrayContaining([expect.stringContaining("Test timeout of 1ms exceeded while setting up")])
        })
      ]))

      expect(data).toHaveProperty("data[1].scope", " chromium example.spec.js says hello")
      expect(data).toHaveProperty("data[1].name", "says hello")
      expect(data).toHaveProperty("data[1].location", "tests/example.spec.js:9:1")
      expect(data).toHaveProperty("data[1].file_name", "tests/example.spec.js")
      expect(data).toHaveProperty("data[1].result", "failed")
      expect(data).toHaveProperty("data[1].failure_reason", expect.stringContaining("Test timeout of 1ms exceeded while setting up"))
      expect(data).toHaveProperty("data[1].failure_expanded", expect.arrayContaining([
        expect.objectContaining({
          expanded: expect.arrayContaining([expect.stringContaining("Test timeout of 1ms exceeded while setting up")])
        })
      ]))
      expect(stdout).toMatch(/Test Engine .* response/m)
    }, TIMEOUT)
  })

  test('it supports test location prefixes for monorepos', async () => {
    const stdout = await runPlaywright([], { ...env, BUILDKITE_ANALYTICS_LOCATION_PREFIX: "some-sub-dir/" })

    const jsonMatch = stdout.match(/.*Test Engine Sending: ({.*})/m)
    const data = JSON.parse(jsonMatch[1])["data"]

    expect(data).toHaveProperty("run_env.location_prefix", "some-sub-dir/")

    expect(data).toHaveProperty("data[0].location", "some-sub-dir/tests/example.spec.js:3:1")
    expect(data).toHaveProperty("data[1].location", "some-sub-dir/tests/example.spec.js:9:1")
  }, TIMEOUT);

  describe('when test is pass but upload fails', () => {
    beforeAll(() => {
      // This will cause the upload to fail
      env.BUILDKITE_ANALYTICS_BASE_URL = "http://"
    })
    test('it should not throw an error', done => {
      exec("npm test example.spec.js:3", { cwd, env: { ...env, JEST_WORKER_ID: undefined } }, (error, stdout) => {
        expect(error).toBeNull()

        done()
      })
    }, TIMEOUT)
  })
});

describe('examples/playwright in OTel mode', () => {
  let server;
  const baseEnv = {
    BUILDKITE_ANALYTICS_TOKEN: undefined,
    BUILDKITE_TESTS_OTLP_TOKEN: 'relay-token',
    BUILDKITE_ANALYTICS_DEBUG_ENABLED: 'true',
    BUILDKITE_ANALYTICS_KEY: 'playwright-otel-e2e',
    BUILDKITE_ANALYTICS_BASE_URL: 'http://127.0.0.1:1/v1/uploads',
    BUILDKITE_ANALYTICS_LOCATION_PREFIX: undefined,
    BUILDKITE_AGENT_ID: 'agent-e2e',
    JEST_WORKER_ID: undefined,
    PLAYWRIGHT_OTEL_ENABLED: undefined,
  };

  const run = async (args = [], env = {}) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'playwright-otel-'));
    const output = path.join(dir, 'results.json');
    try {
      const result = await new Promise(resolve => {
        execFile(process.execPath, [require.resolve('@playwright/test/cli'), 'test', '--config', 'otel.config.js', ...args], {
          cwd,
          env: { ...process.env, ...baseEnv, BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url, ...env, PLAYWRIGHT_JSON_OUTPUT_NAME: output },
        }, (error, stdout, stderr) => resolve({ code: error ? error.code : 0, stdout, stderr }));
      });
      return { ...result, report: JSON.parse(fs.readFileSync(output, 'utf8')) };
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  };

  const jsonRecords = stdout => JSON.parse(stdout.match(/Test Engine Sending: ({.*})/m)[1]).data.data;
  const identity = record => JSON.stringify([record.scope, record.name, record.location, record.file_name]);
  const spanRecord = span => ({
    scope: span.attributes['buildkite.test.scope'],
    name: span.attributes['buildkite.test.name'],
    location: `${span.attributes['code.file.path']}:${span.attributes['code.line.number']}:${span.attributes['code.column.number']}`,
    file_name: span.attributes['code.file.path'],
  });
  const specs = suite => [...(suite.specs || []), ...(suite.suites || []).flatMap(specs)];

  afterEach(async () => {
    if (server) await server.close();
    server = null;
  });

  test.each(['', 'packages/web'])('exports all attempts with JSON identity parity (prefix %j)', async prefix => {
    server = await startOtlpServer({ respond: index => index === 0 ? 503 : 200 });
    const env = { BUILDKITE_ANALYTICS_LOCATION_PREFIX: prefix };
    const result = await run([], env);
    expect(result.code).toBe(1);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);

    const spans = server.spans();
    expect(spans).toHaveLength(13);
    expect(new Set(spans.map(span => span.attributes['buildkite.test.execution.external_id'])).size).toBe(13);
    expect(server.requests[0].status).toBe(503);
    for (const request of server.requests) {
      expect(request.decodeError).toBeNull();
      expect(request.headers).toMatchObject({
        authorization: 'Token token="relay-token"',
        'buildkite-tests-run-key': 'playwright-otel-e2e',
        'buildkite-tests-span-stream': 'test',
        'content-type': 'application/x-protobuf',
        'content-encoding': 'gzip',
      });
    }
    for (const span of spans) {
      expect(span.name).toBe('test.execution');
      expect(span.parentSpanId).toBe('');
      expect(span.attributes['buildkite.execution.via']).toBe('otlp');
      expect(span.attributes['buildkite.run_key']).toBe('playwright-otel-e2e');
      expect(span.attributes['buildkite.test.name']).not.toBe('');
      expect(span.attributes['buildkite.test.framework.name']).toBe('playwright');
      expect(span.attributes['buildkite.test.framework.version']).toBe(require('playwright/package.json').version);
      expect(span.attributeTypes['code.line.number']).toBe('int');
      expect(span.attributeTypes['code.column.number']).toBe('int');
      expect(span.startTimeUnixNano > 0n).toBe(true);
      expect(span.endTimeUnixNano >= span.startTimeUnixNano).toBe(true);
      expect(span.attributes['buildkite.test.execution.external_id']).toMatch(/^[0-9a-f-]{14}4[0-9a-f-]{21}$/);
      expect(span.attributes['buildkite.tag.hello']).toBe('playwright');
    }

    const statuses = name => spans.filter(span => span.attributes['buildkite.test.name'] === name).map(span => span.attributes['test.case.result.status']).sort();
    expect(statuses('passes with tags')).toEqual(['pass']);
    expect(statuses('fails')).toEqual(['fail']);
    expect(statuses('skipped')).toEqual(['skipped']);
    expect(statuses('passes on retry')).toEqual(['fail', 'pass']);
    expect(statuses('expected failure')).toEqual(['fail']);
    expect(statuses('unexpected pass')).toEqual(['pass']);
    expect(statuses('times out')).toEqual(['fail']);
    expect(statuses('first')).toEqual(['fail']);
    expect(statuses('second')).toEqual(['skipped']);
    expect(statuses('body passes')).toEqual(['fail']);
    expect(statuses('parameter 3')).toEqual(['pass']);
    expect(statuses('parameter 7')).toEqual(['pass']);

    const tagged = spans.find(span => span.attributes['buildkite.test.name'] === 'passes with tags');
    expect(tagged.attributes['buildkite.tag.priority']).toBe('test');
    expect(tagged.attributes['buildkite.tag.ci.worker.id']).toBe('test-worker');
    expect(tagged.attributes).not.toHaveProperty('buildkite.tag.ignored');
    const failure = spans.find(span => span.attributes['buildkite.test.name'] === 'fails');
    expect(failure.attributes['buildkite.tag.ci.worker.id']).toBe('configured-worker');
    expect(failure.status).toEqual({ code: 2, message: 'Error: intentional failure' });
    expect(failure.events).toEqual([expect.objectContaining({ name: 'exception', attributes: { 'exception.message': expect.stringContaining('intentional failure') } })]);

    // Compare with the framework's own timings from this same run, not a second execution.
    for (const spec of specs(result.report)) {
      const attempts = spec.tests.flatMap(test => test.results);
      const actual = spans.filter(span => span.attributes['buildkite.test.name'] === spec.title);
      expect(actual.map(span => [Number(span.startTimeUnixNano / 1000000n), Number((span.endTimeUnixNano - span.startTimeUnixNano) / 1000000n)]).sort())
        .toEqual(attempts.map(attempt => [Date.parse(attempt.startTime), attempt.duration]).sort());
    }

    const json = await run(['--config', 'otel-off.config.js'], { ...env, BUILDKITE_ANALYTICS_TOKEN: 'json-token' });
    const records = jsonRecords(json.stdout);
    expect(json.code).toBe(result.code);
    expect(records).toHaveLength(13);
    expect(spans.map(spanRecord).map(identity).sort()).toEqual(records.map(identity).sort());
  }, 60000);

  test('ignores hostile OTEL configuration without losing results or tags', async () => {
    server = await startOtlpServer();
    const result = await run([], {
      OTEL_EXPORTER_OTLP_HEADERS: 'authorization=garbage,x-env-only=1',
      OTEL_EXPORTER_OTLP_TRACES_HEADERS: 'authorization=garbage2,x-env-traces=1',
      OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: 'http/json',
      OTEL_TRACES_SAMPLER: 'always_off',
      OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT: '2',
    });
    expect(result.code).toBe(1);
    expect(server.spans()).toHaveLength(13);
    for (const request of server.requests) {
      expect(request.headers.authorization).toBe('Token token="relay-token"');
      expect(request.headers).not.toHaveProperty('x-env-only');
      expect(request.headers).not.toHaveProperty('x-env-traces');
      expect(request.decodeError).toBeNull();
    }
    expect(server.spans().every(span => span.attributes['buildkite.tag.hello'] === 'playwright')).toBe(true);
  }, 30000);

  test.each([['false', false], [1, false], ['TRUE', true]])('honors explicit OTel enablement %j', async (value, enabled) => {
    server = await startOtlpServer();
    const result = await run(['--grep', ' fails$'], {
      PLAYWRIGHT_OTEL_ENABLED: JSON.stringify(value),
      BUILDKITE_ANALYTICS_TOKEN: 'json-token',
    });
    expect(result.code).toBe(1);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    if (enabled) {
      expect(server.spans()).toHaveLength(1);
      expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    } else {
      expect(server.requests).toHaveLength(0);
      expect(jsonRecords(result.stdout)).toHaveLength(1);
    }
  }, 30000);

  test('uses the JSON path without an OTel warning when no credential is available', async () => {
    server = await startOtlpServer();
    const result = await run(['--grep', ' fails$'], { BUILDKITE_TESTS_OTLP_TOKEN: undefined });
    expect(result.code).toBe(1);
    expect(specs(result.report)).toHaveLength(1);
    expect(result.stderr).toMatch(/Missing BUILDKITE_ANALYTICS_TOKEN/);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(server.requests).toHaveLength(0);
  }, 30000);

  test.each([
    { BUILDKITE_ANALYTICS_KEY: 'invalid key' },
    { BUILDKITE_ANALYTICS_OTLP_ENDPOINT: 'not a URL' },
  ])('falls back to JSON when setup fails: %j', async env => {
    server = await startOtlpServer();
    const result = await run(['--grep', ' fails$'], { ...env, BUILDKITE_ANALYTICS_TOKEN: 'json-token' });
    expect(result.code).toBe(1);
    expect(result.stderr).toMatch(/OpenTelemetry span export disabled:/);
    expect(result.stderr).toMatch(/uploading results as JSON instead/);
    expect(jsonRecords(result.stdout)).toHaveLength(1);
    expect(server.requests).toHaveLength(0);
  }, 30000);

  test.each([['passes with tags', 0], [' fails$', 1]])('reports missing results at an unreachable endpoint for %s, preserving exit %i', async (grep, code) => {
    server = await startOtlpServer();
    const result = await run(['--grep', grep], { BUILDKITE_ANALYTICS_OTLP_ENDPOINT: 'http://127.0.0.1:1/v1/traces' });
    expect(result.code).toBe(code);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(result.stderr).toMatch(/TEST RESULTS MISSING: OpenTelemetry dropped 1 test\.execution span\(s\) \(export-failure, last OTLP failure: ECONNREFUSED\)/);
  }, 60000);

  test('exports an interrupted attempt as skipped and preserves the SIGINT exit code', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-interrupted.config.js']);
    expect(result.code).toBe(130);
    expect(specs(result.report)[0].tests[0].results[0].status).toBe('interrupted');
    expect(server.spans()).toHaveLength(1);
    expect(server.spans()[0].attributes['test.case.result.status']).toBe('skipped');
    expect(server.spans()[0].status.code).toBe(0);
    expect(server.spans()[0].events).toEqual([]);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);

    const json = await run(['--config', 'otel-interrupted.config.js', '--reporter', 'buildkite-test-collector/playwright/reporter,json'], { BUILDKITE_ANALYTICS_TOKEN: 'json-token' });
    expect(json.code).toBe(130);
    expect(jsonRecords(json.stdout)[0].result).toBe('unknown');
  }, 30000);

  test('loads no SDK with OTel off, or before onBegin with OTel requested', async () => {
    const script = `
      const Reporter = require('buildkite-test-collector/playwright/reporter');
      new Reporter({ otelEnabled: true });
      new Reporter({}).onBegin({ version: '1.56.1' });
      console.log(JSON.stringify(Object.keys(require.cache).filter(key => key.includes('@opentelemetry'))));
    `;
    const stdout = await new Promise((resolve, reject) => {
      execFile(process.execPath, ['-e', script], { cwd, env: { ...process.env, ...baseEnv } }, (error, out) => error ? reject(error) : resolve(out));
    });
    expect(JSON.parse(stdout.trim())).toEqual([]);
  });
});
