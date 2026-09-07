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
  onTestRunEnd(testModules) {
    const testResults = testModules.map((testModule) => {
      const tests = Array.from(testModule.children.allTests());
      const diagnostics = tests.map((test) => test.diagnostic());
      const firstStart = diagnostics.reduce((start, diagnostic) =>
        Math.min(start, diagnostic?.startTime ?? Infinity), Infinity);
      const startTime = firstStart === Infinity ? this._start : firstStart;
      const endTime = diagnostics.reduce((end, diagnostic) => diagnostic
        ? Math.max(end, diagnostic.startTime + diagnostic.duration)
        : end, startTime);

      return {
        name: testModule.moduleId,
        startTime,
        endTime,
        assertionResults: tests.map((test, index) => {
          const ancestorTitles = [];
          for (let parent = test.parent; parent.type !== 'module'; parent = parent.parent) {
            ancestorTitles.unshift(parent.name);
          }
          const result = test.result();
          return {
            ancestorTitles,
            title: test.name,
            status: result.state === 'skipped' && test.options.mode === 'todo' ? 'todo' : result.state,
            location: test.location,
            duration: diagnostics[index]?.duration,
            failureMessages: (result.errors || []).map((error) => error.stack || error.message),
          };
        }),
      };
    });

    return this.uploadReport({ startTime: this._start, testResults });
  }

  async uploadReport(report) {
    const originStart = report.startTime;
    const testResults = report.testResults.flatMap((testResult) => {
      const prefixedTestPath = this._paths.prefixTestPath(testResult.name);
      const assertionResults = testResult.assertionResults.map(
        (assertionResult) => {
          const id = randomUUID();

          return {
            id: id,
            scope: assertionResult.ancestorTitles.join(' ').trim(),
            name: assertionResult.title,
            location: (prefixedTestPath && assertionResult.location)
              ? `${prefixedTestPath}:${assertionResult.location.line}`
              : null,
            file_name: prefixedTestPath,
            result: this.testEngineResult(assertionResult),
            failure_reason: this.testEngineFailureReason(assertionResult),
            failure_expanded: this.testEngineFailureExpanded(assertionResult),
            history: this.testEngineHistory(originStart, testResult, assertionResult),
          };
        },
      );

      return assertionResults;
    });

    return uploadTestResults(
      this._testEnv,
      this._tags,
      testResults,
      this._options,
    );
  }

  testEngineResult(assertionResults) {
    /*
     * https://github.com/vitest-dev/vitest/blob/33b930a12feb9f8932b10ed9e41e078200f62379/packages/vitest/src/node/reporters/json.ts#L22
     * vitest test statuses:
     * - failed
     * - pending
     * - passed
     * - skipped
     * - todo
     *
     * Buildkite Test Engine execution results:
     * - passed
     * - failed
     * - pending
     * - skipped
     * - unknown
     */
    return {
      failed: 'failed',
      pending: 'pending',
      passed: 'passed',
      skipped: 'skipped',
      todo: 'pending',
    }[assertionResults.status];
  }

  testEngineFailureMessages(assertionResults) {
    // Strip ANSI color codes from messages and split each line
    return assertionResults.failureMessages.join(' ').replace(/\u001b[^m]*?m/g,'').split("\n")
  }

  testEngineFailureReason(assertionResults) {
    return this.testEngineFailureMessages(assertionResults)[0]
  }

  testEngineFailureExpanded(assertionResults) {
    return [
      {
        expanded: this.testEngineFailureMessages(assertionResults).splice(1),
      },
    ];
  }

  testEngineHistory(originStart, testResult, assertionResults) {
    return {
      section: 'top',
      start_at: (testResult.startTime - originStart) / 1000,
      end_at: (testResult.endTime - originStart) / 1000,
      duration: assertionResults.duration / 1000,
    };
  }
}

// Keep the legacy lifecycle for Vitest 1–2. Resolve this entrypoint only on those
// versions: it was removed in Vitest 5, including from Vite's import resolver.
let Reporter = VitestBuildkiteTestEngineReporter;
if (Number(version.split('.')[0]) < 3) {
  const reportersModule = 'vitest/reporters';
  const { JsonReporter } = await import(reportersModule);
  Reporter = class extends JsonReporter {
    constructor(options) {
      super(options);
      this._collector = new VitestBuildkiteTestEngineReporter(options);
    }

    onInit(ctx) {
      super.onInit(ctx);
      this._collector.onInit(ctx);
    }

    writeReport(reportString) {
      return this._collector.uploadReport(JSON.parse(reportString));
    }
  };
}

export default Reporter;
