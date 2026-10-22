// The smallest transformer that makes `export default` loadable: enough to
// stand in for Babel or ts-jest without adding either to the example.
module.exports = {
  process(src) {
    return { code: src.replace(/^export default /m, 'module.exports = ') }
  },
}
