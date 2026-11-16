const http = require('http')
const api = require('@opentelemetry/api')
const express = require('express')

const port = Number(process.env.PLAYWRIGHT_APP_PORT)
const thirdPartyPort = Number(process.env.PLAYWRIGHT_APP_THIRD_PARTY_PORT)
const tracer = api.trace.getTracer('playwright-e2e-app')

const app = express()
app.get('/health', (req, res) => res.send('ok'))
app.get('/redirect', (req, res) => res.redirect(`http://localhost:${thirdPartyPort}/headers`))
app.get('/', (req, res) => res.type('html').send('<!doctype html><div id="api">pending</div><div id="third">pending</div><script src="/app.js"></script>'))
app.get('/app.js', (req, res) => res.type('js').send(`
  const marker = new URLSearchParams(location.search).get('marker')
  fetch('/api/items/' + marker).then(r => r.json()).then(item => { document.getElementById('api').textContent = item.marker })
  fetch('http://localhost:${thirdPartyPort}/widget').then(r => r.text(), () => 'blocked').then(text => { document.getElementById('third').textContent = text })
`))
app.get('/api/items/:marker', (req, res) => {
  tracer.startActiveSpan('load item', span => {
    span.setAttribute('app.marker', req.params.marker)
    setTimeout(() => {
      span.end()
      res.setHeader('x-app-fixture', req.headers['x-app-fixture'] || 'missing')
      res.setHeader('x-custom', req.headers['x-custom'] || 'missing')
      res.json({ marker: req.params.marker })
    }, 20)
  })
})
app.listen(port, '127.0.0.1')

// Another origin. The headers endpoint observes what each browser sends after a redirect.
http.createServer((req, res) => {
  res.setHeader('access-control-allow-origin', '*')
  if (req.method === 'OPTIONS') {
    // WebKit preflights Authorization after a redirect even when it strips the
    // header. Deliberately do not allow x-preflight-probe, used by the CORS test.
    res.setHeader('access-control-allow-headers', 'authorization')
    res.statusCode = 204
    return res.end()
  }
  if (req.url === '/headers') {
    res.setHeader('content-type', 'application/json')
    return res.end(JSON.stringify({ authorization: req.headers.authorization ?? null, contentType: req.headers['content-type'] ?? null, traceparent: req.headers.traceparent ?? null }))
  }
  res.end('ok')
}).listen(thirdPartyPort, '127.0.0.1')
