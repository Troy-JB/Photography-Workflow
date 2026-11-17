// Static file server with a small reverse proxy, used by gallery-e2e.sh to
// serve the production frontend builds the same way nginx would: the SPA and
// the API share one origin. Test harness only.
//
// usage: node e2e-server.mjs <dist-dir> <port> <api-target> [proxy-prefixes]
import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import path from 'node:path'

const [distArg, portArg, apiArg, prefixArg = '/api/'] = process.argv.slice(2)
if (!distArg || !portArg || !apiArg) {
  console.error('usage: node e2e-server.mjs <dist-dir> <port> <api-target> [prefixes]')
  process.exit(2)
}

const dist = path.resolve(distArg)
const port = Number(portArg)
const api = apiArg.replace(/\/+$/, '')
const prefixes = prefixArg
  .split(',')
  .map((value) => value.trim())
  .filter(Boolean)

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.map': 'application/json',
}

async function readBody(req) {
  const chunks = []
  for await (const chunk of req) chunks.push(chunk)
  return chunks.length ? Buffer.concat(chunks) : undefined
}

async function proxy(req, res, body) {
  const headers = new Headers()
  for (const [key, value] of Object.entries(req.headers)) {
    if (key === 'host' || key === 'connection' || key === 'content-length' || key === 'accept-encoding') continue
    if (Array.isArray(value)) headers.set(key, value.join(', '))
    else if (value !== undefined) headers.set(key, value)
  }
  const upstream = await fetch(api + req.url, { method: req.method, headers, body })
  const payload = Buffer.from(await upstream.arrayBuffer())
  res.statusCode = upstream.status
  for (const [key, value] of upstream.headers) {
    if (key === 'content-length' || key === 'content-encoding' || key === 'transfer-encoding') continue
    res.setHeader(key, value)
  }
  res.setHeader('content-length', String(payload.length))
  res.end(payload)
}

async function serveStatic(req, res) {
  const url = new URL(req.url, 'http://localhost')
  let pathname
  try {
    pathname = decodeURIComponent(url.pathname)
  } catch {
    res.statusCode = 400
    res.end('bad request')
    return
  }
  let file = path.resolve(dist, '.' + pathname)
  if (!file.startsWith(dist + path.sep) && file !== dist) {
    res.statusCode = 403
    res.end('forbidden')
    return
  }
  if (pathname.endsWith('/')) file = path.join(file, 'index.html')
  let data
  try {
    data = await readFile(file)
  } catch {
    // Hash routing keeps every real route at "/"; any unknown path is a stale
    // deep link, so returning index.html is correct for this SPA.
    file = path.join(dist, 'index.html')
    try {
      data = await readFile(file)
    } catch {
      res.statusCode = 404
      res.end('not found')
      return
    }
  }
  res.setHeader('content-type', MIME[path.extname(file).toLowerCase()] ?? 'application/octet-stream')
  res.setHeader('cache-control', 'no-store')
  res.end(req.method === 'HEAD' ? undefined : data)
}

const server = createServer(async (req, res) => {
  try {
    if (prefixes.some((prefix) => (req.url ?? '').startsWith(prefix))) {
      await proxy(req, res, req.method === 'GET' || req.method === 'HEAD' ? undefined : await readBody(req))
      return
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') {
      res.statusCode = 405
      res.end('method not allowed')
      return
    }
    await serveStatic(req, res)
  } catch (err) {
    console.error('e2e-server:', err)
    if (!res.headersSent) {
      res.statusCode = 502
      res.end('bad gateway')
    } else {
      res.end()
    }
  }
})

server.listen(port, '127.0.0.1', () => {
  console.log(`e2e-server: ${dist} on http://127.0.0.1:${port} -> ${api} (${prefixes.join(', ')})`)
})
