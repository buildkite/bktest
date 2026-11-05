declare namespace JestBuildkiteTestEngineReporter {
  interface Options {
    otelEnabled?: boolean
    token?: string
    tags?: Record<string, unknown>
    url?: string
  }
}
declare class JestBuildkiteTestEngineReporter {
  constructor(globalConfig: unknown, options?: JestBuildkiteTestEngineReporter.Options)
}
export = JestBuildkiteTestEngineReporter
