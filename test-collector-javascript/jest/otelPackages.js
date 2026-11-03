const fs = require('fs')
const path = require('path')
const { isBuiltin, createRequire } = require('module')
const semver = require('semver')

const PACKAGES = [
  ['express', 'ExpressInstrumentation'],
  ['graphql', 'GraphQLInstrumentation'],
  ['redis', 'RedisInstrumentation'],
]

// Mirror require-in-the-middle's package/file names, retaining file identity
// even when an entry point re-exports an internal file (Express).
const describe = (from, name) => {
  const file = require.resolve(name.startsWith('.') ? path.resolve(path.dirname(from), name) : name, { paths: [path.dirname(from)] })
  const segments = file.split(path.sep)
  const index = segments.lastIndexOf('node_modules')
  if (index < 0) return null
  const width = segments[index + 1].startsWith('@') ? 2 : 1
  const basedir = segments.slice(0, index + 1 + width).join(path.sep)
  const packageName = segments.slice(index + 1, index + 1 + width).join('/')
  return {
    path: file,
    name: packageName,
    file: require.resolve(packageName, { paths: [basedir] }) === file ? packageName : path.join(packageName, path.relative(basedir, file)),
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

  load(from, name, load) {
    if (!name || isBuiltin(name) || !this.entries.length) return load()
    // Avoid resolving and reading package.json for unrelated imports.
    const candidate = name.startsWith('.') ? from : name
    if (!/(^|[/\\])(express|graphql|redis|@redis|@node-redis)([/\\]|$)/.test(candidate)) return load()
    let record
    const key = `${from}\0${name}`
    if (!this.resolved.has(key)) {
      try { this.resolved.set(key, describe(from, name)) } catch { this.resolved.set(key, null) }
    }
    record = this.resolved.get(key)
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
