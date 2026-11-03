// Runs the fixture files in path order, so the e2e test controls which file a
// process runs first. Self-contained, so it suits every Jest version.
module.exports = class {
  sort(tests) {
    return [...tests].sort((a, b) => a.path.localeCompare(b.path))
  }

  cacheResults() {}
}
