const { trace } = require('@opentelemetry/api')
const { Writable } = require('stream')
const pino = require('pino')
const winston = require('winston')
const bunyan = require('bunyan')

const check = (name, record) => {
  const enabled = (process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS || '').split(',').includes(name) &&
    !(process.env.OTEL_NODE_DISABLED_INSTRUMENTATIONS || '').split(',').includes(name)
  const context = trace.getActiveSpan().spanContext()
  const correlation = Object.fromEntries(['trace_id', 'span_id', 'trace_flags'].filter((key) => key in record).map((key) => [key, record[key]]))
  expect(correlation).toEqual(enabled ? { trace_id: context.traceId, span_id: context.spanId, trace_flags: '01' } : {})
}

test('pino log output', () => {
  const lines = []
  pino({ base: null, timestamp: false }, { write: (line) => lines.push(line) }).info('fixture')
  check('pino', JSON.parse(lines[0]))
  if (!process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS) expect(lines).toEqual(['{"level":30,"msg":"fixture"}\n'])
})

test('winston log output', () => {
  const lines = []
  const stream = new Writable({ write(chunk, _, done) { lines.push(JSON.parse(chunk)); done() } })
  const logger = winston.createLogger({ format: winston.format.json(), transports: [new winston.transports.Stream({ stream })] })
  logger.info('fixture')
  check('winston', lines[0])
  logger.close()
})

test('bunyan log output', () => {
  const records = []
  bunyan.createLogger({ name: 'fixture', streams: [{ type: 'raw', stream: { write: (record) => records.push(record) } }] }).info('fixture')
  check('bunyan', records[0])
})
