import { version } from 'vitest/package.json'
import { randomUUID } from 'node:crypto'
import CI from '../util/ci.js'
import uploadTestResults from '../util/uploadTestResults.js'
import Paths from '../util/paths'

class VitestBuildkiteTestEngineReporter {
  constructor(options) {
    this._options = options;
    this._testEnv = new CI().env('vitest');
    this._tags = options?.tags;
  }

  onInit(ctx) {
    this._start = Date.now()
    this._paths = new Paths({ rootDir: ctx.config.root }, this._testEnv.location_prefix)
  }

  // Vitest 3+ exposes test results through the public reporter API. In Vitest 5,
  // JsonReporter no longer calls writeReport, so subclassing it loses uploads.
  async onTestRunEnd(testModules) {
    const originStart = this._start;
    const testResults = testModules.flatMap((testModule) => {
      const prefixedTestPath = this._paths.prefixTestPath(testModule.moduleId);
      const tests = Array.from(testModule.children.allTests());
      const { startTime, endTime } = this.fileTimings(tests, originStart);

      return tests.map((test) => {
        const result = test.result();
        const failureMessages = this.testEngineFailureMessages(result);

        return {
          id: randomUUID(),
          scope: this.ancestorTitles(test).join(' ').trim(),
          name: test.name,
          location: (prefixedTestPath && test.location)
            ? `${prefixedTestPath}:${test.location.line}`
            : null,
          file_name: prefixedTestPath,
          result: result.state === 'skipped' && test.options.mode === 'todo' ? 'pending' : result.state,
          failure_reason: failureMessages[0],
          failure_expanded: [{ expanded: failureMessages.slice(1) }],
          history: {
            section: 'top',
            start_at: (startTime - originStart) / 1000,
            end_at: (endTime - originStart) / 1000,
            // Preserve null in JSON for tests with no execution timing.
            duration: test.diagnostic()?.duration / 1000,
          },
        };
      });
    });

    return uploadTestResults(
      this._testEnv,
      this._tags,
      testResults,
      this._options,
    );
  }

  ancestorTitles(test) {
    const titles = [];
    let parent = test.parent;
    while (parent.type === 'suite') {
      titles.unshift(parent.name);
      parent = parent.parent;
    }
    return titles;
  }

  // Match the JSON reporter's file-wide window, falling back to the run start
  // when no tests in the file executed.
  fileTimings(tests, originStart) {
    let startTime = Number.POSITIVE_INFINITY;
    let endTime = 0;

    for (const test of tests) {
      const diagnostic = test.diagnostic();
      if (!diagnostic) continue;
      startTime = Math.min(startTime, diagnostic.startTime);
      endTime = Math.max(endTime, diagnostic.startTime + diagnostic.duration);
    }

    if (startTime === Number.POSITIVE_INFINITY) startTime = originStart;
    return { startTime, endTime: Math.max(endTime, startTime) };
  }

  testEngineFailureMessages(result) {
    // Strip ANSI color codes from messages and split each line
    return (result.errors || [])
      .map((error) => error.stack || error.message)
      .join(' ')
      .replace(/\u001b[^m]*?m/g, '')
      .split("\n");
  }
}

// Freeze the original implementation for Vitest 1–2. Its removed import must
// never be loaded on Vitest 5.
export default Number(version.split('.')[0]) < 3
  ? (await import('./legacy-reporter.js')).default
  : VitestBuildkiteTestEngineReporter;
