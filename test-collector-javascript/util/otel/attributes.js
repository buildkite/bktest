const workerIdTag = require('../workerIdTag')

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

const resourceAttributes = (runEnv) => {
  const pipelineRun = ciPipelineRun(runEnv)
  const workerId = process.env.BUILDKITE_AGENT_ID

  return {
    'service.name': process.env.BUILDKITE_TEST_ENGINE_SUITE_SLUG,
    'service.namespace': process.env.BUILDKITE_ORGANIZATION_SLUG,
    'cicd.pipeline.run.id': pipelineRun.id,
    'cicd.pipeline.run.url.full': pipelineRun.id ? pipelineRun.url : undefined,
    'cicd.worker.id': workerId?.trim() ? workerId : undefined,
    'process.runtime.version': process.versions.node,
    'vcs.ref.head.name': runEnv.branch,
    'vcs.ref.head.revision': runEnv.commit_sha,
    'vcs.ref.type': runEnv.branch ? (process.env.BUILDKITE_TAG ? 'tag' : 'branch') : undefined,
  }
}

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
    ...Object.fromEntries(Object.entries({ ...workerIdTag(), ...tags }).map(([key, value]) => [`buildkite.tag.${key}`, value])),
  }
}

module.exports = { resourceAttributes, runAttributes }
