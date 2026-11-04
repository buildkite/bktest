const fs = require('fs')
const path = require('path')
const { isBuiltin } = require('module')
const semver = require('semver')

// Keep file identity when a package re-exports an internal file, as Express does.
const packageForFile = (file, runtime, names) => {
  const segments = file.split(path.sep)
  const index = segments.lastIndexOf('node_modules')
  if (index < 0) return null
  const width = segments[index + 1].startsWith('@') ? 2 : 1
  const basedir = segments.slice(0, index + 1 + width).join(path.sep)
  const packageName = segments.slice(index + 1, index + 1 + width).join('/')
  if (!names.has(packageName)) return null
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
  constructor(context, entries) {
    this.context = context
    this.entries = entries.map((entry) => ({ ...entry,
      modules: new Set(entry.instance.getModuleDefinitions().map((definition) => definition.name).filter((name) => !isBuiltin(name))),
    })).filter((entry) => entry.modules.size)
    this.names = new Set(this.entries.flatMap((entry) => [...entry.modules]))
    this.instances = new Map()
    this.loading = new Set()
    this.loads = new WeakMap()
    this.resolved = new Map()
  }

  load(file, load) {
    if (!this.names.size) return load()
    if (!this.resolved.has(file)) {
      try { this.resolved.set(file, packageForFile(file, this.context.runtime, this.names)) } catch { this.resolved.set(file, null) }
    }
    const record = this.resolved.get(file)
    if (!record || this.loading.has(record.path)) return load()
    this.loading.add(record.path)
    let exports
    try { exports = load() } finally { this.loading.delete(record.path) }
    if (!exports || !['object', 'function'].includes(typeof exports)) return exports
    let files = this.loads.get(exports)
    if (!files) this.loads.set(exports, files = new Map())
    if (files.has(record.path)) return files.get(record.path)
    // Mark before constructing instrumentation, which also loads packages.
    files.set(record.path, exports)
    for (const { name, className, resolved, modules } of this.entries) {
      if (!modules.has(record.name)) continue
      try {
        if (!this.instances.has(name)) {
          this.instances.set(name, null)
          const { context } = this
          const api = context.runtime.requireModuleOrMock(resolved, '@opentelemetry/api')
          if (typeof api?.trace?.getSpan !== 'function' || typeof api?.context?.with !== 'function' ||
              typeof api?.context?.active !== 'function' || typeof api.context.active()?.getValue !== 'function') {
            this.names.clear()
            context.warnOnce(`package-api:${context.testPath}`, 'OpenTelemetry package instrumentation skipped because its API is mocked or incomplete; test.execution export remains enabled.')
            return exports
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
        // Redis's composite exposes child definitions here, not in _modules.
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
