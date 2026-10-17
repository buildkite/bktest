const workerIdTag = require('../workerIdTag')

const TAG_PREFIX = 'buildkite.tag.'

const isBlank = (value) => value === undefined || value === null || String(value).trim() === ''

// The CI provider's own run id and URL, for correlation with other CI
// telemetry. The Test Engine run key is a separate identity.
const ciPipelineRun = (runEnv) => {
  switch (runEnv.ci) {
    case 'buildkite':
      return { id: process.env.BUILDKITE_BUILD_ID, url: process.env.BUILDKITE_BUILD_URL }
    case 'github_actions': {
      const id = process.env.GITHUB_RUN_ID
      const repository = process.env.GITHUB_REPOSITORY
      const url = (id && repository) ? `https://github.com/${repository}/actions/runs/${id}` : undefined
      return { id, url }
    }
    case 'circleci':
      return { id: process.env.CIRCLE_WORKFLOW_ID, url: undefined }
    default:
      return { id: undefined, url: undefined }
  }
}

// Undefined values are fine here and below: the SDK skips them when they
// become resource or span attributes.
const resourceAttributes = (runEnv) => {
  const pipelineRun = ciPipelineRun(runEnv)
  const workerId = process.env.BUILDKITE_AGENT_ID

  return {
    'service.name': process.env.BUILDKITE_TEST_ENGINE_SUITE_SLUG,
    'service.namespace': process.env.BUILDKITE_ORGANIZATION_SLUG,
    'cicd.pipeline.run.id': pipelineRun.id,
    'cicd.pipeline.run.url.full': pipelineRun.id ? pipelineRun.url : undefined,
    'cicd.worker.id': isBlank(workerId) ? undefined : workerId,
    'process.runtime.version': process.versions.node,
    'vcs.ref.head.name': runEnv.branch,
    'vcs.ref.head.revision': runEnv.commit_sha,
    'vcs.ref.type': runEnv.branch ? (process.env.BUILDKITE_TAG ? 'tag' : 'branch') : undefined,
  }
}

const tagAttributes = (tags) => {
  const attributes = {}
  for (const [key, value] of Object.entries(tags)) {
    attributes[`${TAG_PREFIX}${key}`] = value
  }
  return attributes
}

// Run metadata the server reads from each test span, plus the run-level tags.
const runAttributes = ({ runEnv, tags, testRunner, frameworkVersion }) => {
  const pipelineRun = ciPipelineRun(runEnv)

  return {
    'buildkite.run_key': runEnv.key,
    'buildkite.build_number': runEnv.number,
    'buildkite.job_id': runEnv.job_id,
    'buildkite.message': runEnv.message,
    'buildkite.step_id': process.env.BUILDKITE_STEP_ID,
    'buildkite.collector.name': runEnv.collector,
    'buildkite.collector.version': runEnv.version,
    'buildkite.location_prefix': runEnv.location_prefix,
    'buildkite.test.framework.name': testRunner,
    'buildkite.test.framework.version': frameworkVersion,
    'buildkite.run_url': runEnv.url && runEnv.url !== pipelineRun.url ? runEnv.url : undefined,
    ...tagAttributes({ ...workerIdTag(), ...tags }),
  }
}

module.exports = { resourceAttributes, runAttributes }
