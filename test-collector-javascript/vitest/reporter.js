import { randomUUID } from 'node:crypto'
import CI from '../util/ci.js'
import uploadTestResults from '../util/uploadTestResults.js'
import Paths from '../util/paths'

/*
 * A Vitest reporter built on the public reporter API
 * https://vitest.dev/advanced/api/reporters
 *
 * It deliberately imports nothing from `vitest` at runtime. Earlier versions
 * subclassed Vitest's internal JsonReporter (via the `vitest/reporters`
 * entrypoint) and intercepted its `writeReport` method. Vitest 5 removed that
 * entrypoint and inlined `writeReport`, so that approach both fails to load and,
 * once the import is repointed, silently stops uploading. Working from the
 * `TestModule` / `TestCase` objects handed to `onTestRunEnd` is stable from
 * Vitest 3.0 onwards and lets one code path support 3.x, 4.x and 5.x.
 */
class VitestBuildkiteTestEngineReporter {
  constructor(options) {
    this._options = options;
    this._testEnv = new CI().env('vitest');
    this._tags = options?.tags;
  }

  onInit(ctx) {
    this._start = Date.now();
    this._paths = new Paths({ rootDir: ctx.config.root }, this._testEnv.location_prefix);
  }

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
          result: this.testEngineResult(test, result),
          failure_reason: failureMessages[0],
          failure_expanded: [{ expanded: failureMessages.slice(1) }],
          history: {
            section: 'top',
            start_at: (startTime - originStart) / 1000,
            end_at: (endTime - originStart) / 1000,
            duration: (test.diagnostic()?.duration ?? 0) / 1000,
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

  /*
   * Names of the enclosing describe blocks, outermost first. Stops at the
   * test module (file) itself, which is the root of the parent chain.
   */
  ancestorTitles(test) {
    const titles = [];
    let parent = test.parent;
    while (parent && parent.type === 'suite') {
      titles.unshift(parent.name);
      parent = parent.parent;
    }
    return titles;
  }

  /*
   * The window in which a file's tests ran, matching what Vitest's own JSON
   * reporter computes: earliest test start to the latest test end. Files
   * whose tests never ran (all skipped) fall back to the run start.
   */
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

  testEngineResult(test, result) {
    /*
     * Vitest TestResult states:
     * - passed
     * - failed
     * - skipped (includes todo tests)
     * - pending (collected but not run)
     *
     * Buildkite Test Engine execution results:
     * - passed
     * - failed
     * - pending
     * - skipped
     * - unknown
     */
    switch (result.state) {
      case 'passed': return 'passed';
      case 'failed': return 'failed';
      case 'pending': return 'pending';
      default:
        // Report todo tests as pending, as the JSON reporter used to
        return test.task?.mode === 'todo' ? 'pending' : 'skipped';
    }
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

export default VitestBuildkiteTestEngineReporter;
