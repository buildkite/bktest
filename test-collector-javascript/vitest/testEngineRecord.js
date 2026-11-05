module.exports = (test, fileName) => {
  const titles = []
  for (let parent = test.parent; parent.type === 'suite'; parent = parent.parent) titles.unshift(parent.name)
  const failures = (test.result().errors || [])
    .map(error => error.stack || error.message).join(' ')
    .replace(/\u001b[^m]*?m/g, '').split('\n')
  const result = test.result()
  return {
    scope: titles.join(' ').trim(), name: test.name,
    location: fileName && test.location ? `${fileName}:${test.location.line}` : null,
    file_name: fileName,
    result: result.state === 'skipped' && test.options.mode === 'todo' ? 'pending' : result.state,
    failure_reason: failures[0], failure_expanded: [{ expanded: failures.slice(1) }],
  }
}
