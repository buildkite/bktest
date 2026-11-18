const fs = require('fs')
const path = require('path')
const Module = require('module')
const { pathToFileURL } = require('url')
const semver = require('semver')
const { describeError } = require('../util/otel/warn')

const DOCS = 'https://github.com/buildkite/bktest/blob/main/test-collector-javascript/docs/opentelemetry.md#vitest-package-instrumentation'

const isPackageEntry = (entry) => entry.instance.getModuleDefinitions().some((definition) => !Module.isBuiltin(definition.name))

// Hooks fire only for the import-in-the-middle copy whose loader wrapped a
// module, so use the copy each instrumentation's base resolves.
const loaderFor = (resolved) => {
  const load = Module.createRequire(resolved)
  const iitm = path.dirname(Module.createRequire(load.resolve('@opentelemetry/instrumentation')).resolve('import-in-the-middle'))
  return {
    hook: load.resolve('@opentelemetry/instrumentation/hook.mjs'),
    iitm,
    version: JSON.parse(fs.readFileSync(path.join(iitm, 'package.json'), 'utf8')).version,
  }
}

const escape = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const exportTargets = (value) => typeof value === 'string' ? [value] : value && typeof value === 'object' ? Object.values(value).flatMap(exportTargets) : []

// The files Vite may import as a package's entry point: its CommonJS main, the
// ES module build Vite picks for an extensionless "main" such as GraphQL's
// "index" (vitest-dev/vitest#8414), and every target of its "exports" root.
const entriesOf = (name, baseDir) => {
  let main, manifest
  try { main = Module.createRequire(path.join(baseDir, 'package.json')).resolve(name) } catch { /* No CommonJS entry. */ }
  try { manifest = JSON.parse(fs.readFileSync(path.join(baseDir, 'package.json'), 'utf8')) } catch { return undefined }
  const { exports } = manifest
  const root = exports && typeof exports === 'object' && Object.keys(exports).some((key) => key.startsWith('.')) ? exports['.'] : exports
  const esm = !exports && main?.endsWith('.js') ? `${main.slice(0, -3)}.mjs` : undefined
  const files = new Set([main, esm, ...exportTargets(root).map((target) => path.join(baseDir, target))].filter(Boolean))
  return { manifest, main, esm, files }
}

// The package names, as OpenTelemetry's own hook passes, plus the entry files
// Vitest imports by path. Wrap only entry modules, as import 'pkg' does:
// import-in-the-middle copies a wrapped module's exports, so later
// reassignments never reach importers (openai 4 then fails to import).
const includeFor = (names) => {
  const lookup = Module.createRequire(path.join(process.cwd(), 'package.json'))
  const entries = names.flatMap((name) => {
    const found = lookup.resolve.paths(name)?.map((dir) => path.join(dir, name)).find((dir) => fs.existsSync(path.join(dir, 'package.json')))
    if (!found) return []
    const baseDir = fs.realpathSync(found)
    const files = [...entriesOf(name, baseDir)?.files ?? []].map((file) => path.relative(baseDir, file).split(path.sep).join('/'))
    return files.length ? [`${escape(name)}/(?:${files.map(escape).join('|')})`] : []
  })
  return entries.length ? [...names, new RegExp(`/node_modules/(?:${entries.join('|')})(?:[?#]|$)`)] : names
}

const register = ({ hook, iitm }, include) => {
  if (typeof Module.registerHooks === 'function') {
    let hooks
    try { hooks = require(path.join(iitm, 'register-hooks.mjs')) } catch { /* Added in import-in-the-middle 3.1. */ }
    // In-thread hooks avoid module.register, which Node 26 deprecates at runtime (DEP0205).
    if (hooks?.supportsSyncHooks?.()) {
      // Node's synchronous hooks pass conditions to the CommonJS resolver.
      // Older Yarn PnP rejects that option. Check before installing a hook:
      // catching an activation error afterwards leaves require() broken.
      require.resolve('./otelPackages', { conditions: ['node', 'require'] })
      hooks.register({ include })
      return 'synchronous'
    }
  }
  Module.register(pathToFileURL(hook).href, { data: { include } })
  return 'asynchronous'
}

const buildsOf = (name, baseDir, definitions) => {
  const entries = entriesOf(name, baseDir)
  if (!entries) return {}
  return {
    ...entries,
    supported: definitions.filter((definition) => definition.supportedVersions.some((range) =>
      semver.satisfies(entries.manifest.version, range, { includePrerelease: definition.includePrerelease }))),
  }
}

// import-in-the-middle reports a package's main module by name and other files
// as name/relative/path. Warn when Vitest loaded a file the patches cannot reach.
const watchUnpatched = (loader, packages, warnOnce) => {
  const { Hook } = require(loader.iitm)
  const names = [...packages.keys()]
  const builds = new Map()
  // Runs while the user's import evaluates, so a failure here must not fail the import.
  new Hook(names, { internals: true }, (namespace, name, baseDir) => {
    try {
      const packageName = names.find((candidate) => name.startsWith(`${candidate}${path.sep}`))
      if (!packageName || !baseDir) return
      const { instrumentations, definitions } = packages.get(packageName)
      if (!builds.has(baseDir)) builds.set(baseDir, buildsOf(packageName, baseDir, definitions))
      const { main, esm, files, supported } = builds.get(baseDir)
      if (!supported?.length) return
      const file = path.join(baseDir, name.slice(packageName.length + 1))
      const by = [...instrumentations].join(', ')
      if (files.has(file) && supported.every((definition) => definition.patch && !definition.files?.length)) {
        warnOnce(`vitest-package-path:${packageName}`, `${by} cannot patch ${packageName} in Vitest: Vitest imported its entry point by file path (${file}), which import-in-the-middle ${loader.version} does not treat as an import of ${packageName}. ` +
          `ES module and TypeScript files that import ${packageName} get no ${packageName} spans; require() still does, including through createRequire (${DOCS}); test.execution export remains enabled.`)
      } else if (file === esm && !supported.some((definition) => definition.files?.some((patched) => path.normalize(patched.name) === name))) {
        warnOnce(`vitest-package-build:${packageName}`, `${by} cannot patch ${packageName} in Vitest: Vitest loaded its ES module build (${path.relative(baseDir, file)}), ` +
          `not the CommonJS build Node loads (${path.relative(baseDir, main)}), which the instrumentation patches. Resolve ${packageName} and its subpaths to the CommonJS build to record its spans (${DOCS}); test.execution export remains enabled.`)
      }
    } catch (error) {
      warnOnce('vitest-package-watch', `Vitest package instrumentation could not check ${name} (${describeError(error)}); its spans may be missing without a warning; test.execution export remains enabled.`)
    }
  })
}

// Vitest imports packages by the file path Vite resolved, through Node's ESM
// loader, so require-in-the-middle sees only their CommonJS internals. Register
// OpenTelemetry's standard import-in-the-middle loader for the selected packages.
const activatePackages = (entries, tracerProvider, warnOnce) => {
  const loaders = new Map()
  for (const entry of entries) {
    let loader
    try { loader = loaderFor(entry.resolved) } catch (error) {
      warnOnce(`vitest-package-loader:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} is disabled in Vitest: its import-in-the-middle loader was not found (${describeError(error)}); test.execution export remains enabled.`)
      continue
    }
    if (!loaders.has(loader.iitm)) loaders.set(loader.iitm, { loader, entries: [] })
    loaders.get(loader.iitm).entries.push(entry)
  }
  // A second loader would wrap modules twice. Installs usually dedupe to one copy.
  const [selected, ...others] = [...loaders.values()].sort((a, b) => b.entries.length - a.entries.length)
  if (!selected) return undefined
  for (const entry of others.flatMap((other) => other.entries)) {
    warnOnce(`vitest-package-copy:${entry.name}`, `OpenTelemetry instrumentation ${entry.name} is disabled in Vitest: it uses a different import-in-the-middle copy than ${selected.entries.map((other) => other.name).join(', ')}. ` +
      'Deduplicate @opentelemetry/instrumentation (for example, npm dedupe); test.execution export remains enabled.')
  }
  const packages = new Map()
  for (const { instance } of selected.entries) {
    for (const definition of instance.getModuleDefinitions()) {
      if (Module.isBuiltin(definition.name)) continue
      if (!packages.has(definition.name)) packages.set(definition.name, { instrumentations: new Set(), definitions: [] })
      packages.get(definition.name).instrumentations.add(instance.instrumentationName)
      packages.get(definition.name).definitions.push(definition)
    }
  }
  const mode = register(selected.loader, includeFor([...packages.keys()]))
  for (const { instance } of selected.entries) {
    instance.setTracerProvider(tracerProvider)
    instance.setConfig({ ...instance.getConfig(), enabled: true })
    instance.enable()
  }
  watchUnpatched(selected.loader, packages, warnOnce)
  return { names: selected.entries.map((entry) => entry.name), loader: `import-in-the-middle ${selected.loader.version}, ${mode}` }
}

module.exports = { activatePackages, isPackageEntry }
