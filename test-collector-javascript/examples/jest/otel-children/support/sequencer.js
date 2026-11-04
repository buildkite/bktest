// Runs named projects and then fixture files in order, so the e2e test
// controls which project/file a process runs first. Suits every Jest version.
module.exports = class {
  sort(tests) {
    return [...tests].sort((a, b) =>
      (a.context.config.displayName?.name || '').localeCompare(b.context.config.displayName?.name || '') || a.path.localeCompare(b.path))
  }

  cacheResults() {}
}
