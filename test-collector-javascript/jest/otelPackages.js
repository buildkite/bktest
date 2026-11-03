const fs = require('fs')
const path = require('path')
const { createRequire } = require('module')
const semver = require('semver')

const PACKAGES = [
  ['express', 'ExpressInstrumentation'],
  ['graphql', 'GraphQLInstrumentation'],
  ['redis', 'RedisInstrumentation'],
]

// Mirror require-in-the-middle's package/file names, retaining file identity
// even when an entry point re-exports an internal file (Express).
const describe = (file, runtime) => {
  const segments = file.split(path.sep)
  const index = segments.lastIndexOf('node_modules')
  if (index < 0) return null
  const width = segments[index + 1].startsWith('@') ? 2 : 1
  const basedir = segments.slice(0, index + 1 + width).join(path.sep)
  const packageName = segments.slice(index + 1, index + 1 + width).join('/')
  const from = path.join(basedir, 'package.json')
  const entry = runtime._resolution ? runtime._resolution.resolveCjs(from, packageName) : runtime._resolveCjsModule(from, packageName)
  return {
    path: file,
    name: packageName,
    file: entry === file ? packageName : path.join(packageName, path.relative(basedir, file)),
    version: JSON.parse(fs.readFileSync(path.join(basedir, 'package.json'), 'utf8')).version,
  }
}

class JestOtelPackages {
  constructor(context, enabledByEnv) {
    this.context = context
    this.entries = PACKAGES.filter(([name]) => enabledByEnv(name))
    this.instances = new Map()
    this.loading = new Set()
    this.loads = new WeakMap()
    this.resolved = new Map()
    this.fromProject = createRequire(path.join(context.rootDir, 'package.json'))
  }

  load(file, load) {
    if (!this.entries.length) return load()
    // Filter the actual Jest-resolved path, never the import specifier.
    if (!/(^|[/\\])node_modules[/\\](express|graphql|redis|@redis|@node-redis)([/\\]|$)/.test(file)) return load()
    let record
    if (!this.resolved.has(file)) {
      try { this.resolved.set(file, describe(file, this.context.runtime)) } catch { this.resolved.set(file, null) }
    }
    record = this.resolved.get(file)
    if (!record || !this.entries.some(([name]) => record.name === name || (name === 'redis' && record.name === '@redis/client')) || this.loading.has(record.path)) return load()
    this.loading.add(record.path)
    let exports
    try { exports = load() } finally { this.loading.delete(record.path) }
    if (!exports || !['object', 'function'].includes(typeof exports)) return exports
    let files = this.loads.get(exports)
    if (!files) this.loads.set(exports, files = new Map())
    if (files.has(record.path)) return files.get(record.path)
    // Mark before constructing instrumentation, which also loads packages.
    files.set(record.path, exports)
    for (const [name, className] of this.entries) {
      if (record.name !== name && !(name === 'redis' && record.name === '@redis/client')) continue
      try {
        if (!this.instances.has(name)) {
          this.instances.set(name, null)
          const { context } = this
          const packageName = `@opentelemetry/instrumentation-${name}`
          let resolved
          try {
            resolved = this.fromProject.resolve(packageName)
          } catch (error) {
            if (error.code !== 'MODULE_NOT_FOUND') throw error
            try {
              resolved = createRequire(this.fromProject.resolve('@opentelemetry/auto-instrumentations-node')).resolve(packageName)
            } catch (error) {
              if (error.code === 'MODULE_NOT_FOUND') continue // optional
              throw error
            }
          }
          const mod = context.runtime.requireModule(context.testPath, resolved)
          const instance = new mod[className]({ enabled: false })
          instance.setTracerProvider(context._worker().tracerProvider)
          instance.setConfig({ ...instance.getConfig(), enabled: true })
          instance.enable()
          this.instances.set(name, instance)
          context.cleanups.push(() => instance.disable())
        }
        const instance = this.instances.get(name)
        // Composite instrumentations (Redis) expose their children's
        // definitions here, while their own _modules is empty.
        for (const definition of instance?.getModuleDefinitions() || []) {
          if (definition.name !== record.name) continue
          const targets = record.file === record.name ? [definition] : (definition.files || []).filter((file) => path.normalize(file.name) === path.normalize(record.file))
          for (const target of targets) {
            if (typeof target.patch === 'function' && target.supportedVersions.some((range) => semver.satisfies(record.version, range, { includePrerelease: definition.includePrerelease }))) {
              exports = target.patch(exports, record.version) ?? exports
            }
          }
        }
      } catch (error) {
        // Optional packages are not a prerequisite for test-result export.
        this.context.warnOnce(`package:${name}`, `OpenTelemetry instrumentation ${name} could not patch ${record.file}: ${error.message.split('\n')[0]}; test.execution export remains enabled.`)
      }
    }
    files.set(record.path, exports)
    return exports
  }
}

module.exports = JestOtelPackages
