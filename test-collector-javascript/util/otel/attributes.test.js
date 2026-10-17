const { resourceAttributes, runAttributes } = require('./attributes')

const OLD_ENV = process.env

beforeEach(() => {
  process.env = { ...OLD_ENV }
  for (const key of Object.keys(process.env)) {
    if (/^(BUILDKITE|GITHUB|CIRCLE)_/.test(key)) delete process.env[key]
  }
})

afterAll(() => {
  process.env = OLD_ENV
})

const buildkiteRunEnv = {
  ci: 'buildkite',
  key: 'build-1',
  url: 'https://buildkite.com/org/pipeline/builds/1',
  branch: 'main',
  commit_sha: 'abc123',
  number: '1',
  job_id: 'job-1',
  message: 'commit message',
  collector: 'js-buildkite-test-collector',
  version: '1.12.0',
}

describe('resourceAttributes', () => {
  it('describes the suite, pipeline run, worker, runtime and VCS ref on Buildkite', () => {
    process.env.BUILDKITE_BUILD_ID = 'build-uuid'
    process.env.BUILDKITE_BUILD_URL = 'https://buildkite.com/org/pipeline/builds/1'
    process.env.BUILDKITE_AGENT_ID = 'agent-1'
    process.env.BUILDKITE_TEST_ENGINE_SUITE_SLUG = 'my-suite'
    process.env.BUILDKITE_ORGANIZATION_SLUG = 'my-org'

    expect(resourceAttributes(buildkiteRunEnv)).toEqual({
      'service.name': 'my-suite',
      'service.namespace': 'my-org',
      'cicd.pipeline.run.id': 'build-uuid',
      'cicd.pipeline.run.url.full': 'https://buildkite.com/org/pipeline/builds/1',
      'cicd.worker.id': 'agent-1',
      'process.runtime.version': process.versions.node,
      'vcs.ref.head.name': 'main',
      'vcs.ref.head.revision': 'abc123',
      'vcs.ref.type': 'branch',
    })
  })

  it('marks a tag build and leaves out what it cannot find', () => {
    process.env.BUILDKITE_TAG = 'v1.0.0'
    process.env.BUILDKITE_AGENT_ID = '  '

    const attributes = resourceAttributes({ ci: 'generic', key: 'k', branch: 'v1.0.0' })

    expect(attributes['vcs.ref.type']).toBe('tag')
    expect(attributes).toEqual({
      'process.runtime.version': process.versions.node,
      'vcs.ref.head.name': 'v1.0.0',
      'vcs.ref.type': 'tag',
    })
  })

  it('derives the GitHub Actions run URL from the repository and run id', () => {
    process.env.GITHUB_RUN_ID = '42'
    process.env.GITHUB_REPOSITORY = 'buildkite/bktest'

    const attributes = resourceAttributes({ ci: 'github_actions', key: 'k' })

    expect(attributes['cicd.pipeline.run.id']).toBe('42')
    expect(attributes['cicd.pipeline.run.url.full']).toBe('https://github.com/buildkite/bktest/actions/runs/42')
  })
})

describe('runAttributes', () => {
  const settings = { runEnv: buildkiteRunEnv, tags: { hello: 'world', count: '3' }, testRunner: 'jest', frameworkVersion: '29.7.0' }

  it('carries the run metadata and tags as buildkite.* attributes', () => {
    process.env.BUILDKITE_BUILD_URL = 'https://buildkite.com/org/pipeline/builds/1'
    process.env.BUILDKITE_STEP_ID = 'step-1'

    expect(runAttributes(settings)).toEqual({
      'buildkite.run_key': 'build-1',
      'buildkite.build_number': '1',
      'buildkite.job_id': 'job-1',
      'buildkite.message': 'commit message',
      'buildkite.step_id': 'step-1',
      'buildkite.collector.name': 'js-buildkite-test-collector',
      'buildkite.collector.version': '1.12.0',
      'buildkite.test.framework.name': 'jest',
      'buildkite.test.framework.version': '29.7.0',
      'buildkite.tag.hello': 'world',
      'buildkite.tag.count': '3',
    })
  })

  it('sends the run URL only when it differs from the CI run URL', () => {
    process.env.BUILDKITE_BUILD_URL = 'https://buildkite.com/org/pipeline/builds/1'
    expect(runAttributes(settings)['buildkite.run_url']).toBeUndefined()

    process.env.BUILDKITE_BUILD_URL = 'https://buildkite.com/org/pipeline/builds/2'
    expect(runAttributes(settings)['buildkite.run_url']).toBe('https://buildkite.com/org/pipeline/builds/1')
  })

  it('tags executions with the agent id unless the caller supplies ci.worker.id', () => {
    process.env.BUILDKITE_AGENT_ID = 'agent-1'

    expect(runAttributes(settings)['buildkite.tag.ci.worker.id']).toBe('agent-1')
    expect(runAttributes({ ...settings, tags: { 'ci.worker.id': 'mine' } })['buildkite.tag.ci.worker.id']).toBe('mine')
  })

  it('includes the location prefix when the run has one', () => {
    expect(runAttributes({ ...settings, runEnv: { ...buildkiteRunEnv, location_prefix: 'packages/a' } })['buildkite.location_prefix']).toBe('packages/a')
  })
})
