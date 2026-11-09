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
    PLAYWRIGHT_OTEL_SETUP_TOKEN: undefined,
    PLAYWRIGHT_OTEL_TEST_MATCH: undefined,
    PLAYWRIGHT_OTEL_SIGNAL: undefined,
    PLAYWRIGHT_OTEL_OTHER_SIGTERM: undefined,
    PLAYWRIGHT_OTEL_TERM_ON_END: undefined,
  };

  const run = async (args = [], env = {}) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'playwright-otel-'));
    const output = path.join(dir, 'results.json');
    try {
      const result = await new Promise(resolve => {
        execFile(process.execPath, [require.resolve('@playwright/test/cli'), 'test', '--config', 'otel.config.js', ...args], {
          cwd,
          timeout: 25000,
          maxBuffer: 16 * 1024 * 1024,
          env: { ...process.env, ...baseEnv, BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url, ...env, PLAYWRIGHT_JSON_OUTPUT_NAME: output },
        }, (error, stdout, stderr) => resolve({ code: error?.signal === 'SIGTERM' ? 143 : error ? error.code : 0, stdout, stderr }));
      });
      return {
        ...result,
        report: fs.existsSync(output) ? JSON.parse(fs.readFileSync(output, 'utf8')) : null,
        steps: fs.existsSync(output + '.steps') ? JSON.parse(fs.readFileSync(output + '.steps', 'utf8')) : [],
        signaledAt: fs.existsSync(output + '.signal') ? Number(fs.readFileSync(output + '.signal', 'utf8')) : null,
      };
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
  const testSpans = () => server.spans().filter(span => span.name === 'test.execution');
  const childSpans = () => server.spans().filter(span => span.name !== 'test.execution');
  const expectRootTimings = result => {
    const stepTimes = steps => steps.flatMap(step => [step.start + Math.max(0, step.duration), ...stepTimes(step.steps)]);
    for (const spec of specs(result.report)) {
      const attempts = spec.tests.flatMap(test => test.results);
      const actual = testSpans().filter(span => span.attributes['buildkite.test.name'] === spec.title);
      const expected = attempts.map(attempt => {
        const start = Date.parse(attempt.startTime);
        const native = result.steps.find(entry => entry.name === spec.title && entry.start === start);
        return [start, Math.max(start + attempt.duration, ...stepTimes(native.steps))];
      });
      expect(actual.map(span => [Number(span.startTimeUnixNano / 1000000n), Number(span.endTimeUnixNano / 1000000n)]).sort())
        .toEqual(expected.sort());
    }
  };

  afterEach(async () => {
    if (server) await server.close();
    server = null;
  });

  test('redacts quoted API values without filtering step categories or user titles', async () => {
    server = await startOtlpServer();
    const result = await run(['--grep', 'API values'], { PLAYWRIGHT_OTEL_TEST_MATCH: 'values.js' });
    expect(result.code).toBe(0);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(testSpans()).toHaveLength(1);
    const children = childSpans();
    const nativeSteps = steps => steps.flatMap(step => [step, ...nativeSteps(step.steps)]);
    const native = result.steps.flatMap(attempt => nativeSteps(attempt.steps));
    expect(children).toHaveLength(native.length);
    const apiSteps = children.filter(span => span.attributes['test.step.category'] === 'pw:api');
    expect(apiSteps).toHaveLength(native.filter(step => step.category === 'pw:api').length);
    expect(apiSteps.some(span => span.name.includes('[REDACTED]'))).toBe(true);
    for (const span of apiSteps) {
      expect(span.name).toBe(span.attributes['test.step.name']);
      expect(span.name).not.toMatch(/hunter2|SECRET|escaped|quote|secret|parenthesized|path|next-line|Shift\+S|private-option|Password/);
    }
    expect(children.map(span => span.attributes['test.step.category'])).toEqual(expect.arrayContaining(['hook', 'fixture', 'test.step', 'pw:api', 'expect']));
    expect(children.some(span => span.name === 'user step "kept verbatim"')).toBe(true);
  }, 30000);

  test('truncates both step error fields so large diffs reach the receiver', async () => {
    server = await startOtlpServer();
    const result = await run(['--grep', 'large step errors'], { PLAYWRIGHT_OTEL_TEST_MATCH: 'values.js' });
    expect(result.code).toBe(1);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(testSpans()).toHaveLength(1);
    const failures = childSpans().filter(span => span.status.code === 2);
    expect(failures.map(span => span.attributes['test.step.category']).sort()).toEqual(['expect', 'test.step']);
    for (const span of failures) {
      expect(span.status.message).toContain('toEqual');
      expect(Array.from(span.status.message)).toHaveLength(10243);
      expect(Array.from(span.events[0].attributes['exception.message'])).toHaveLength(10243);
      expect(span.events[0].attributes['exception.message']).toMatch(/\.\.\.$/);
    }
    expect(server.requests.every(request => request.bytes <= 750 * 1024)).toBe(true);
  }, 30000);

  test('records open attempts before the globalTimeout drain without duplicates', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-shutdown.config.js']);
    expect(result.code).toBe(1);
    expect(testSpans()).toHaveLength(7);
    expect(testSpans().filter(span => span.attributes['test.case.result.status'] === 'pass')).toHaveLength(6);
    const interrupted = testSpans().find(span => span.attributes['buildkite.test.name'] === 'open during shutdown');
    expect(interrupted.attributes['test.case.result.status']).toBe('skipped');
    expect(interrupted.status.code).toBe(0);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(new Set(testSpans().map(span => span.spanId)).size).toBe(7);
  }, 30000);

  test('bounds the wait for a tagless open attempt and ignores its late end', async () => {
    server = await startOtlpServer();
    const script = `
      const Reporter = require('buildkite-test-collector/playwright/reporter');
      const reporter = new Reporter({ otelEnabled: true });
      const test = { id: 'test', title: 'unfinished', titlePath: () => ['suite'], location: { file: __filename, line: 1, column: 1 } };
      const result = { startTime: new Date(), duration: -1, status: 'skipped' };
      (async () => {
        reporter.onBegin({ version: '1.41.2' });
        reporter.onTestBegin(test, result);
        await reporter.onEnd({ status: 'timedout' });
        result.status = 'failed';
        reporter.onTestEnd(test, result);
        await reporter.onEnd({ status: 'timedout' });
      })().catch(error => { console.error(error); process.exitCode = 1; });
    `;
    const start = Date.now();
    const result = await new Promise(resolve => {
      execFile(process.execPath, ['-e', script], {
        cwd, timeout: 10000,
        env: { ...process.env, ...baseEnv, BUILDKITE_ANALYTICS_OTLP_ENDPOINT: server.url },
      }, (error, stdout, stderr) => resolve({ error, stderr }));
    });
    expect(result).toEqual({ error: null, stderr: '' });
    expect(Date.now() - start).toBeLessThan(4000);
    expect(testSpans()).toHaveLength(1);
    expect(testSpans()[0].attributes['test.case.result.status']).toBe('skipped');
    expect(testSpans()[0].status.code).toBe(0);
  }, 15000);

  test.each([false, true])('drains completed results on SIGTERM (receiver hangs: %s)', async hangs => {
    server = await startOtlpServer({ respond: () => hangs ? { hang: true } : 200 });
    const result = await run(['--config', 'otel-shutdown.config.js'], { PLAYWRIGHT_OTEL_SIGNAL: 'SIGTERM' });
    expect(result.code).toBe(143);
    expect(result.signaledAt).not.toBeNull();
    expect(Date.now() - result.signaledAt).toBeLessThan(6500);
    // A hung receiver still saw the data, but never acknowledged it.
    const received = server.requests.flatMap(request => request.decoded.resourceSpans.flatMap(resource => resource.scopeSpans.flatMap(scope => scope.spans)));
    expect(received.filter(span => span.name === 'test.execution' && span.attributes['test.case.result.status'] === 'pass')).toHaveLength(6);
    if (hangs) expect(result.stderr).toMatch(/TEST RESULTS MISSING: 7 test.execution span\(s\)/);
    else {
      expect(testSpans()).toHaveLength(7);
      expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    }
  }, 30000);

  test('shortens an existing final drain to five seconds on SIGTERM', async () => {
    server = await startOtlpServer({ respond: () => ({ hang: true }) });
    const result = await run(['--grep', ' fails$'], { PLAYWRIGHT_OTEL_TERM_ON_END: '1' });
    expect(result.code).toBe(143);
    expect(result.signaledAt).not.toBeNull();
    expect(Date.now() - result.signaledAt).toBeLessThan(6500);
    expect(result.stderr).toMatch(/TEST RESULTS MISSING: 1 test.execution span\(s\)/);
  }, 30000);

  test('preserves asynchronous cleanup owned by another SIGTERM listener', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-shutdown.config.js'], {
      PLAYWRIGHT_OTEL_SIGNAL: 'SIGTERM', PLAYWRIGHT_OTEL_OTHER_SIGTERM: '1',
    });
    expect(result.code).toBe(42);
    expect(result.stdout).toContain('other SIGTERM cleanup finished');
    expect(testSpans()).toHaveLength(7);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
  }, 30000);

  test('exports the native step tree with distinct nested, repeated and parallel children', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-steps.config.js'], { BUILDKITE_ANALYTICS_LOCATION_PREFIX: 'packages/web' });
    expect(result.code).toBe(0);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    const roots = testSpans();
    const children = childSpans();
    expect(roots).toHaveLength(7);
    expect(new Set(server.spans().map(span => span.spanId)).size).toBe(server.spans().length);
    expect(new Set(roots.map(span => span.traceId)).size).toBe(7);
    const sort = values => values.sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
    const nativeTree = steps => sort(steps.map(step => ({ ...step, title: step.category === 'pw:api' ? '<API>' : step.title, steps: nativeTree(step.steps) })));
    const visited = new Set();
    const exportedTree = parent => sort(children.filter(span => span.parentSpanId === parent.spanId).map(span => {
      visited.add(span.spanId);
      expect(span.traceId).toBe(parent.traceId);
      expect(span.endTimeUnixNano >= span.startTimeUnixNano).toBe(true);
      expect(span.startTimeUnixNano >= parent.startTimeUnixNano).toBe(true);
      expect(span.endTimeUnixNano <= parent.endTimeUnixNano).toBe(true);
      if (span.attributes['code.file.path']) expect(span.attributes['code.file.path']).toMatch(/^packages\/web\//);
      return {
        title: span.attributes['test.step.category'] === 'pw:api' ? '<API>' : span.name,
        category: span.attributes['test.step.category'],
        start: Number(span.startTimeUnixNano / 1000000n),
        duration: Number((span.endTimeUnixNano - span.startTimeUnixNano) / 1000000n),
        error: span.status.code === 2,
        steps: exportedTree(span),
      };
    }));
    expect(result.steps).toHaveLength(7);
    for (const attempt of result.steps) {
      const root = roots.find(span => span.attributes['buildkite.test.name'] === attempt.name && Number(span.startTimeUnixNano / 1000000n) === attempt.start);
      expect(root).toBeDefined();
      expect(exportedTree(root)).toEqual(nativeTree(attempt.steps));
    }
    expect(visited.size).toBe(children.length); // No orphan or unrelated worker spans.
    expectRootTimings(result);
    expect(children.map(span => span.attributes['test.step.category']))
      .toEqual(expect.arrayContaining(['hook', 'fixture', 'test.step', 'pw:api', 'expect']));
    for (const name of ['nested parallel 1', 'nested parallel 2']) {
      const root = roots.find(span => span.attributes['buildkite.test.name'] === name);
      const repeated = children.filter(span => span.traceId === root.traceId && span.name === 'repeated');
      expect(repeated).toHaveLength(4);
      expect(children.filter(span => span.traceId === root.traceId && span.name === 'leaf')).toHaveLength(2);
    }
    expect(roots.filter(span => span.attributes['buildkite.test.name'] === 'retry child').map(span => span.attributes['test.case.result.status']).sort()).toEqual(['fail', 'pass']);
    const failedStep = children.find(span => span.name === 'retry step' && span.status.code === 2);
    expect(failedStep.events[0]).toMatchObject({ name: 'exception', attributes: { 'exception.message': expect.stringContaining('toBe') } });
    expect(roots.filter(span => ['skip inside step', 'static skip'].includes(span.attributes['buildkite.test.name'])).map(span => span.attributes['test.case.result.status'])).toEqual(['skipped', 'skipped']);
  }, 30000);

  test('caps children per file across parallel attempts and retries without dropping test results', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-steps.config.js'], { BUILDKITE_TESTS_OTEL_CHILD_SPAN_QUEUE_SIZE: '3' });
    expect(result.code).toBe(0);
    expect(testSpans()).toHaveLength(7);
    const children = childSpans();
    expect(children).toHaveLength(6); // Two files, not a new allowance for each worker or attempt.
    const other = testSpans().find(span => span.attributes['buildkite.test.name'] === 'another file');
    expect(children.filter(span => span.traceId === other.traceId)).toHaveLength(3);
    expectRootTimings(result); // Bounds must include steps even when child export is capped.
    expect(result.stderr.match(/OpenTelemetry dropped \d+ child span\(s\) in total \(per-file cap 3\)/g)).toHaveLength(1);
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING|Error in reporter/);
  }, 30000);

  test('keeps test results when the receiver rejects only the child stream', async () => {
    server = await startOtlpServer({ respond: (index, headers) => headers['buildkite-tests-span-stream'] === 'child' ? 403 : 200 });
    const result = await run(['--grep', 'passes with tags']);
    expect(result.code).toBe(0);
    expect(testSpans()).toHaveLength(1);
    expect(childSpans()).toHaveLength(0);
    expect(result.stderr).toMatch(/OpenTelemetry dropped \d+ child span\(s\)/);
    expect(result.stderr).not.toMatch(/TEST RESULTS MISSING|Error in reporter/);
  }, 30000);

  test.each(['', 'packages/web'])('exports all attempts with JSON identity parity (prefix %j)', async prefix => {
    server = await startOtlpServer({ respond: index => index === 0 ? 503 : 200 });
    const env = { BUILDKITE_ANALYTICS_LOCATION_PREFIX: prefix };
    const result = await run([], env);
    expect(result.code).toBe(1);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);

    const spans = testSpans();
    expect(spans).toHaveLength(13);
    expect(new Set(spans.map(span => span.attributes['buildkite.test.execution.external_id'])).size).toBe(13);
    expect(server.requests[0].status).toBe(503);
    for (const request of server.requests) {
      expect(request.decodeError).toBeNull();
      expect(request.headers).toMatchObject({
        authorization: 'Token token="relay-token"',
        'buildkite-tests-run-key': 'playwright-otel-e2e',
        'content-type': 'application/x-protobuf',
        'content-encoding': 'gzip',
      });
      const stream = request.headers['buildkite-tests-span-stream'];
      expect(['test', 'child']).toContain(stream);
      for (const resource of request.decoded.resourceSpans) {
        for (const scope of resource.scopeSpans) {
          expect(scope.spans.every(span => (span.name === 'test.execution') === (stream === 'test'))).toBe(true);
        }
      }
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
    const supportsTags = require('semver').gte(require('@playwright/test/package.json').version, '1.42.0');
    expect(tagged.attributes['buildkite.tag.priority']).toBe(supportsTags ? 'test' : 'run');
    expect(tagged.attributes['buildkite.tag.ci.worker.id']).toBe(supportsTags ? 'test-worker' : 'configured-worker');
    expect(tagged.attributes).not.toHaveProperty('buildkite.tag.ignored');
    const failure = spans.find(span => span.attributes['buildkite.test.name'] === 'fails');
    expect(failure.attributes['buildkite.tag.ci.worker.id']).toBe('configured-worker');
    expect(failure.status).toEqual({ code: 2, message: 'Error: intentional failure' });
    expect(failure.events).toEqual([expect.objectContaining({ name: 'exception', attributes: { 'exception.message': expect.stringContaining('intentional failure') } })]);

    // Keep the framework start, extending its reported duration to enclose native steps.
    expectRootTimings(result);

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
    expect(testSpans()).toHaveLength(13);
    expect(childSpans()).toHaveLength(0); // Children honour the environment's always_off sampler.
    expectRootTimings(result);
    for (const request of server.requests) {
      expect(request.headers.authorization).toBe('Token token="relay-token"');
      expect(request.headers).not.toHaveProperty('x-env-only');
      expect(request.headers).not.toHaveProperty('x-env-traces');
      expect(request.decodeError).toBeNull();
    }
    expect(testSpans().every(span => span.attributes['buildkite.tag.hello'] === 'playwright')).toBe(true);
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
      expect(testSpans()).toHaveLength(1);
      expect(childSpans().length).toBeGreaterThan(0);
      expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    } else {
      expect(server.requests).toHaveLength(0);
      expect(jsonRecords(result.stdout)).toHaveLength(1);
    }
  }, 30000);

  test('warns once without uploading when no credential is available', async () => {
    server = await startOtlpServer();
    const result = await run(['--grep', ' fails$'], { BUILDKITE_TESTS_OTLP_TOKEN: undefined });
    expect(result.code).toBe(1);
    expect(specs(result.report)).toHaveLength(1);
    expect(result.stderr.match(/\[buildkite-test-collector\]/g)).toHaveLength(1);
    expect(result.stderr).toMatch(/results were not uploaded: set BUILDKITE_TESTS_OTLP_TOKEN/);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(server.requests).toHaveLength(0);
  }, 30000);

  test.each(['BUILDKITE_ANALYTICS_TOKEN', 'BUILDKITE_TESTS_OTLP_TOKEN'])('uploads with %s loaded in globalSetup', async tokenVariable => {
    server = await startOtlpServer();
    const result = await run(['--grep', 'passes with tags'], {
      BUILDKITE_TESTS_OTLP_TOKEN: undefined,
      PLAYWRIGHT_OTEL_SETUP_TOKEN: tokenVariable,
    });
    expect(result.code).toBe(0);
    expect(result.stderr).not.toMatch(/\[buildkite-test-collector\]|Error in reporter/);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(testSpans()).toHaveLength(1);
    expect(testSpans()[0].attributes['buildkite.test.name']).toBe('passes with tags');
    expect(testSpans()[0].attributes['test.case.result.status']).toBe('pass');
    expect(server.requests[0].headers.authorization).toBe('Token token="setup-token"');
  }, 30000);

  test.each([
    { BUILDKITE_ANALYTICS_KEY: 'invalid key' },
    { BUILDKITE_ANALYTICS_OTLP_ENDPOINT: 'not a URL' },
  ])('reports missing results without JSON fallback when setup fails: %j', async env => {
    server = await startOtlpServer();
    const result = await run(['--grep', ' fails$'], { ...env, BUILDKITE_ANALYTICS_TOKEN: 'json-token' });
    expect(result.code).toBe(1);
    expect(result.stderr).toMatch(/OpenTelemetry span export disabled:/);
    expect(result.stderr).toMatch(/no JSON fallback/);
    expect(result.stderr).toMatch(/TEST RESULTS MISSING: 1 Playwright test execution\(s\) were not recorded/);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(server.requests).toHaveLength(0);
  }, 30000);

  test.each([['passes with tags', 0], [' fails$', 1]])('reports missing results at an unreachable endpoint for %s, preserving exit %i', async (grep, code) => {
    server = await startOtlpServer();
    const result = await run(['--grep', grep], { BUILDKITE_ANALYTICS_OTLP_ENDPOINT: 'http://127.0.0.1:1/v1/traces' });
    expect(result.code).toBe(code);
    expect(result.stdout).not.toMatch(/Test Engine Sending:/);
    expect(result.stderr).toMatch(/TEST RESULTS MISSING: 1 test\.execution span\(s\), [1-9]\d* child span\(s\) were not acknowledged \(ECONNREFUSED\)/);
    expect(result.stderr).toMatch(/OpenTelemetry dropped \d+ child span\(s\).*ECONNREFUSED/);
  }, 60000);

  test('exports an interrupted attempt as skipped and preserves the SIGINT exit code', async () => {
    server = await startOtlpServer();
    const result = await run(['--config', 'otel-interrupted.config.js']);
    expect(result.code).toBe(130);
    expect(specs(result.report)[0].tests[0].results[0].status).toBe('interrupted');
    expect(testSpans()).toHaveLength(1);
    const root = testSpans()[0];
    expect(root.attributes['test.case.result.status']).toBe('skipped');
    expect(root.status.code).toBe(0);
    expect(root.events).toEqual([]);
    const outer = childSpans().find(span => span.name === 'interrupted outer');
    const inner = childSpans().find(span => span.name === 'interrupted inner');
    expect(outer.parentSpanId).toBe(root.spanId);
    expect(inner.parentSpanId).toBe(outer.spanId);
    expect(outer.traceId).toBe(root.traceId);
    expect(inner.traceId).toBe(root.traceId);
    for (const span of [outer, inner]) {
      expect(span.attributes['test.step.incomplete']).toBe(true);
      expect(span.startTimeUnixNano <= root.endTimeUnixNano).toBe(true);
      expect(span.endTimeUnixNano).toBe(root.endTimeUnixNano);
      expect(span.status.code).toBe(0);
    }
    expectRootTimings(result);
    const completed = childSpans().find(span => span.name === 'completed before cancellation');
    expect(completed.attributes).not.toHaveProperty('test.step.incomplete');
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
