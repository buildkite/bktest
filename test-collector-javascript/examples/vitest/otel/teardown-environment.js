export default {
  name: 'teardown', transformMode: 'ssr', viteEnvironment: 'ssr',
  async setup() {
    return { async teardown() { await new Promise(resolve => setTimeout(resolve, 300)) } }
  },
}
