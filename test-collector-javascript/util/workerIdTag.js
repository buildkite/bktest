// Tags every execution in the upload with the ID of the agent running it,
// so failures can be grouped by worker. Omitted when the agent doesn't
// expose an ID (e.g. outside Buildkite), so callers can still supply their
// own "ci.worker.id" tag without it being clobbered.
const workerIdTag = () => {
  const agentId = process.env.BUILDKITE_AGENT_ID
  if (!agentId || agentId.trim() === '') return {}

  return { 'ci.worker.id': agentId }
}

module.exports = workerIdTag
