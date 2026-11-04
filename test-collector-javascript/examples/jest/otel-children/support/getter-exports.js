// Like abstract-logging and ansi-styles: every read returns a fresh object,
// and assigning even the unchanged value back to module.exports must fail.
Object.defineProperty(module, 'exports', { get: () => ({ answer: 7 }) })
