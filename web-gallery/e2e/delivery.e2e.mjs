// End-to-end walkthrough of the M7 delivery flow through the real UIs. The
// fixture is seeded through the public API (contract, sandbox payments,
// gallery selection and an approved proof round) so the walkthrough starts
// from a paid project with an approved round. The owner then creates the
// delivery, builds both ZIPs, issues a download link and emails it; the
// customer opens the fragment link on a phone-sized viewport, downloads the
// web package and one original, and loses access the moment the owner revokes
// the token.
import { chromium } from 'playwright'
import { createHash, createHmac } from 'node:crypto'
import { readdirSync, readFileSync } from 'node:fs'
import { deflateSync } from 'node:zlib'

const cfg = {
  api: (process.env.PWF_E2E_API ?? 'http://127.0.0.1:18084').replace(/\/+$/, ''),
  admin: (process.env.PWF_E2E_ADMIN ?? 'http://127.0.0.1:4175').replace(/\/+$/, ''),
  gallery: (process.env.PWF_E2E_GALLERY ?? 'http://127.0.0.1:4174').replace(/\/+$/, ''),
  email: process.env.PWF_E2E_EMAIL ?? 'owner@example.com',
  password: process.env.PWF_E2E_PASSWORD ?? 'photographer123',
  mailDir: process.env.PWF_E2E_MAIL_DIR ?? '',
  shots: process.env.PWF_E2E_SHOTS ?? '',
  esignSecret: process.env.PWF_E2E_ESIGN_SECRET ?? 'dev-only-esign-secret-change-me',
  paymentSecret: process.env.PWF_E2E_PAYMENT_SECRET ?? 'dev-only-payment-secret-change-me',
  merchant: process.env.PWF_E2E_MERCHANT ?? 'sandbox-merchant-001',
}

function check(condition, message) {
  if (!condition) throw new Error(message)
}

async function until(label, fn, { timeout = 90_000, interval = 300 } = {}) {
  const deadline = Date.now() + timeout
  let last
  while (Date.now() < deadline) {
    try {
      last = await fn()
      if (last) return last
    } catch (err) {
      last = err
    }
    await new Promise((resolve) => setTimeout(resolve, interval))
  }
  const detail = last instanceof Error ? `: ${last.message}` : last === undefined ? '' : ` (last value ${JSON.stringify(last)})`
  throw new Error(`timed out waiting for ${label}${detail}`)
}

/* ---------- API + sandbox callbacks ---------- */

async function api(path, { method = 'GET', token, body, headers = {} } = {}) {
  const response = await fetch(cfg.api + path, {
    method,
    headers: {
      ...(body !== undefined ? { 'content-type': 'application/json' } : {}),
      ...(token ? { authorization: `Bearer ${token}` } : {}),
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  const text = await response.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`)
  return data
}

// signCallback mirrors payment.SignRequestBody / esign.SignRequestBody:
// HMAC-SHA256 over "<unix>.<raw body>" with the sandbox secret.
function signCallback(secret, raw) {
  const ts = Math.floor(Date.now() / 1000)
  const signature = createHmac('sha256', secret).update(String(ts)).update('.').update(raw).digest('hex')
  return `t=${ts},v1=${signature}`
}

async function callback(path, secret, body) {
  const raw = Buffer.from(JSON.stringify(body))
  const response = await fetch(cfg.api + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-pwf-signature': signCallback(secret, raw) },
    body: raw,
  })
  const text = await response.text()
  check(response.ok, `callback ${body.event_id}: ${response.status} ${text}`)
  let data = {}
  try {
    data = JSON.parse(text)
  } catch {
    // a non-JSON 2xx still counts as accepted
  }
  check(!data.status || data.status === 'processed', `callback ${body.event_id}: ${text}`)
}

/* ---------- PNG fixtures ---------- */

const CRC_TABLE = (() => {
  const table = new Uint32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    table[n] = c >>> 0
  }
  return table
})()

function crc32(buffer) {
  let c = 0xffffffff
  for (const byte of buffer) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}

function pngChunk(type, data) {
  const name = Buffer.from(type, 'ascii')
  const length = Buffer.alloc(4)
  length.writeUInt32BE(data.length)
  const crc = Buffer.alloc(4)
  crc.writeUInt32BE(crc32(Buffer.concat([name, data])))
  return Buffer.concat([length, name, data, crc])
}

function makePNG(width, height, index) {
  const raw = Buffer.alloc((width * 3 + 1) * height)
  for (let y = 0; y < height; y++) {
    const row = y * (width * 3 + 1)
    raw[row] = 0
    for (let x = 0; x < width; x++) {
      const offset = row + 1 + x * 3
      raw[offset] = (index * 61 + Math.floor((x * 255) / width)) & 0xff
      raw[offset + 1] = (90 + Math.floor((y * 165) / height)) & 0xff
      raw[offset + 2] = (200 - index * 40) & 0xff
    }
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(width, 0)
  ihdr.writeUInt32BE(height, 4)
  ihdr[8] = 8
  ihdr[9] = 2
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', deflateSync(raw, { level: 9 })),
    pngChunk('IEND', Buffer.alloc(0)),
  ])
}

/* ---------- paid + approved fixture ---------- */

async function seedPaidApprovedProject(token) {
  const stamp = new Date().toISOString().slice(11, 19)
  const title = `Delivery E2E ${stamp}`
  const clientEmail = `delivery-e2e+${Date.now()}@example.com`
  const client = await api('/api/v1/clients', {
    method: 'POST',
    token,
    body: { name: 'Delivery E2E Customer', email: clientEmail },
  })
  const project = await api('/api/v1/projects', {
    method: 'POST',
    token,
    body: {
      title,
      client_id: client.id,
      total_amount_cents: 300000,
      deposit_cents: 100000,
      balance_cents: 200000,
    },
  })

  const quote = await api('/api/v1/quotes', {
    method: 'POST',
    token,
    body: {
      project_id: project.id,
      deposit_cents: 100000,
      items: [{ kind: 'service', description: '交付 E2E 拍摄', quantity: 1, unit_price_cents: 300000 }],
    },
  })
  const sentQuote = await api(`/api/v1/quotes/${quote.id}/send`, {
    method: 'POST',
    token,
    body: { expected_version: quote.row_version },
  })
  const contract = await api('/api/v1/contracts', {
    method: 'POST',
    token,
    body: { project_id: project.id, quote_id: sentQuote.id, signer_name: 'Delivery E2E Customer' },
  })
  const signing = await api(`/api/v1/contracts/${contract.id}/send`, {
    method: 'POST',
    token,
    body: { expected_version: contract.version },
  })
  await callback('/webhooks/esign/sandbox', cfg.esignSecret, {
    event_id: `delivery-e2e-signing-${Date.now()}`,
    sign_task_id: signing.sign_task_id,
    type: 'signing',
  })
  await callback('/webhooks/esign/sandbox', cfg.esignSecret, {
    event_id: `delivery-e2e-signed-${Date.now()}`,
    sign_task_id: signing.sign_task_id,
    type: 'signed',
    signer_name: 'Delivery E2E Customer',
    document_sha256: 'e'.repeat(64),
    signed_at: new Date().toISOString(),
  })
  const signedContract = await api(`/api/v1/contracts/${contract.id}`, { token })
  check(signedContract.status === 'signed', `contract status ${signedContract.status}, want signed`)

  for (const [kind, amount] of [
    ['deposit', 100000],
    ['balance', 200000],
  ]) {
    const order = await api('/api/v1/payments/orders', {
      method: 'POST',
      token,
      body: { project_id: project.id, kind },
    })
    check(order.amount_cents === amount, `${kind} order amount ${order.amount_cents}, want ${amount}`)
    await callback('/webhooks/payment/sandbox', cfg.paymentSecret, {
      event_id: `delivery-e2e-${kind}-${Date.now()}`,
      type: 'payment.succeeded',
      merchant_id: cfg.merchant,
      out_trade_no: order.out_trade_no,
      provider_trade_no: `e2e-${kind}-${Date.now()}`,
      amount_cents: amount,
      currency: 'CNY',
      paid_at: new Date().toISOString(),
    })
  }
  const paid = await api(`/api/v1/projects/${project.id}`, { token })
  check(paid.payment_status === 'paid', `project payment_status ${paid.payment_status}, want paid`)

  const width = 800
  const height = 600
  const files = []
  const images = []
  for (let i = 0; i < 2; i++) {
    const bytes = makePNG(width, height, i)
    images.push(bytes)
    files.push({
      rel_path: `delivery-e2e/IMG_${String(i + 1).padStart(4, '0')}.PNG`,
      size: bytes.length,
      sha256: createHash('sha256').update(bytes).digest('hex'),
      mime: 'image/png',
      width,
      height,
    })
  }
  const batch = await api('/agent/v1/ingest-batches', {
    method: 'POST',
    token,
    body: { project_id: project.id, device_id: 'delivery-e2e' },
  })
  const registered = await api(`/agent/v1/ingest-batches/${batch.id}/assets/batch`, {
    method: 'POST',
    token,
    body: { files },
  })
  check(registered.accepted === 2, `registered ${registered.accepted} files, want 2`)
  const assetIDs = []
  for (let i = 0; i < registered.assets.length; i++) {
    const assetID = registered.assets[i].asset_id
    assetIDs.push(assetID)
    const plan = await api('/agent/v1/uploads/presign', {
      method: 'POST',
      token,
      body: { asset_id: assetID, size: images[i].length },
    })
    check(plan.mode === 'single', `upload mode ${plan.mode}, want single`)
    const put = await fetch(plan.url, { method: 'PUT', body: images[i] })
    check(put.ok, `PUT fixture ${i} failed: ${put.status}`)
    await api(`/agent/v1/uploads/${assetID}/complete`, {
      method: 'POST',
      token,
      body: { upload_id: plan.upload_id, parts: [] },
    })
  }
  await api(`/agent/v1/ingest-batches/${batch.id}/finalize`, { method: 'POST', token })
  await until(
    'ingest verification',
    async () => {
      const view = await api(`/agent/v1/ingest-batches/${batch.id}`, { token })
      if (view.verify_job?.status === 'failed') throw new Error(`verify job failed: ${view.verify_job.last_error}`)
      return view.assets?.verified === 2 && view.assets?.missing === 0
    },
    { timeout: 120_000 },
  )

  const gallery = await api('/api/v1/galleries', {
    method: 'POST',
    token,
    body: {
      project_id: project.id,
      title,
      expires_at: new Date(Date.now() + 2 * 3600_000).toISOString(),
      selection_limit: 2,
    },
  })
  const attached = await api(`/api/v1/galleries/${gallery.id}/assets`, {
    method: 'POST',
    token,
    body: { asset_ids: assetIDs },
  })
  check(attached.added === 2, `attached ${attached.added} assets, want 2`)
  await api(`/api/v1/projects/${project.id}/derivatives:derive`, {
    method: 'POST',
    token,
    body: { asset_ids: [], force: false },
  })
  await until(
    'preview derivation',
    async () => {
      const page = await api(`/api/v1/galleries/${gallery.id}/assets?limit=60&offset=0&filter=all`, { token })
      return page.derivatives.missing === 0 && page.derivatives.failed === 0 && page.items.every((item) => item.preview_ready)
    },
    { timeout: 180_000, interval: 1000 },
  )
  await until(
    'gallery publish',
    async () => {
      try {
        await api(`/api/v1/galleries/${gallery.id}/publish`, { method: 'POST', token, body: {} })
        return true
      } catch (err) {
        if (!String(err.message).includes('derivatives_incomplete')) throw err
        return false
      }
    },
    { timeout: 180_000, interval: 2000 },
  )

  const share = await api(`/api/v1/galleries/${gallery.id}/tokens`, {
    method: 'POST',
    token,
    body: { label: 'delivery-e2e' },
  })
  const session = await api('/g/v1/session', { method: 'POST', body: { token: share.token } })
  await api('/g/v1/selections', {
    method: 'POST',
    token: session.session,
    body: {
      expected_version: 0,
      items: assetIDs.map((assetID) => ({ asset_id: assetID, decision: 'selected' })),
    },
  })
  await api(`/g/v1/albums/${gallery.id}/submit`, {
    method: 'POST',
    token: session.session,
    body: { expected_version: 1 },
  })

  const created = await api('/api/v1/proof-rounds', {
    method: 'POST',
    token,
    body: { project_id: project.id, gallery_id: gallery.id, due_at: new Date(Date.now() + 7 * 86400_000).toISOString() },
  })
  check(created.items === 2, `proof round items ${created.items}, want 2`)
  const proofSession = await api('/g/v1/proof/session', {
    method: 'POST',
    body: { token: created.token.token },
  })
  const manifest = await api(`/g/v1/proof/rounds/${created.round.id}/manifest?limit=60`, {
    token: proofSession.session,
  })
  const approved = await api(`/g/v1/proof/rounds/${created.round.id}/submit`, {
    method: 'POST',
    token: proofSession.session,
    body: { decision: 'approved', expected_version: manifest.round.version },
  })
  check(approved.status === 'approved', `proof round ${approved.status}, want approved`)
  console.log(`delivery-e2e: seeded paid project ${project.id} with approved round ${created.round.id}`)
  return { projectID: project.id, roundID: created.round.id, title, clientEmail, images }
}

/* ---------- browser helpers ---------- */

async function launchBrowser() {
  const channel = process.env.PWF_E2E_CHANNEL
  if (channel) return chromium.launch({ channel })
  try {
    return await chromium.launch({ channel: 'chrome' })
  } catch {
    return chromium.launch()
  }
}

function watchPage(page, label, errors) {
  page.on('pageerror', (err) => errors.push(`${label} pageerror: ${err.message}`))
  page.on('console', (message) => {
    if (message.type() === 'error') errors.push(`${label} console: ${message.text()}`)
  })
  page.on('requestfailed', (request) => {
    const reason = request.failure()?.errorText ?? 'unknown'
    if (reason.includes('ERR_ABORTED')) return
    errors.push(`${label} request failed: ${request.method()} ${request.url()} (${reason})`)
  })
}

async function shot(page, name) {
  if (!cfg.shots) return
  try {
    await page.screenshot({ path: `${cfg.shots}/${name}.png`, fullPage: true })
  } catch {
    // screenshots are best-effort diagnostics
  }
}

/* ---------- mail ---------- */

function capturedBodies() {
  if (!cfg.mailDir) return []
  let entries
  try {
    entries = readdirSync(cfg.mailDir)
  } catch {
    return []
  }
  const bodies = []
  for (const name of entries) {
    if (!name.endsWith('.eml')) continue
    const text = readFileSync(`${cfg.mailDir}/${name}`, 'utf8')
    const split = text.indexOf('\r\n\r\n')
    if (split < 0) continue
    const encoded = text.slice(split + 4).replace(/[\r\n]/g, '')
    try {
      bodies.push(Buffer.from(encoded, 'base64').toString('utf8'))
    } catch {
      // a malformed capture is ignored; the timeout below reports it
    }
  }
  return bodies
}

async function waitDeliveryMail() {
  if (!cfg.mailDir) {
    console.log('delivery-e2e: mail check skipped (pass PWF_E2E_MAIL_DIR to inspect SMTP)')
    return
  }
  await until(
    'delivery email in the SMTP sink',
    async () => capturedBodies().some((body) => body.includes('/#/d/')),
    { timeout: 120_000, interval: 500 },
  )
  console.log('delivery-e2e: customer email captured with a fragment delivery link')
}

/* ---------- walkthrough ---------- */

async function run() {
  console.log(`delivery-e2e: fixture via API ${cfg.api}`)
  const token = (await api('/api/v1/auth/login', {
    method: 'POST',
    body: { email: cfg.email, password: cfg.password },
  })).access_token
  check(typeof token === 'string' && token.length > 0, 'login returned no access token')
  const fixture = await seedPaidApprovedProject(token)

  const browser = await launchBrowser()
  const errors = []
  let adminPage
  let customerPage
  let deliveryURL = ''
  try {
    /* ----- owner creates the delivery from the approved round ----- */
    const admin = await browser.newContext({ viewport: { width: 1440, height: 960 }, acceptDownloads: true })
    adminPage = await admin.newPage()
    watchPage(adminPage, 'admin', errors)
    await adminPage.goto(cfg.admin + '/', { waitUntil: 'domcontentloaded' })
    await adminPage.getByLabel('邮箱').fill(cfg.email)
    await adminPage.getByLabel('密码').fill(cfg.password)
    await adminPage.getByRole('button', { name: '登录' }).click()
    await adminPage.getByRole('heading', { name: '选片相册' }).waitFor({ timeout: 30_000 })

    await adminPage.goto(`${cfg.admin}/#/p/${fixture.roundID}`, { waitUntil: 'domcontentloaded' })
    await adminPage.getByRole('heading', { name: '第 1 轮审片' }).waitFor({ timeout: 30_000 })
    await adminPage.locator('.pill').filter({ hasText: '已通过' }).first().waitFor({ timeout: 30_000 })
    await adminPage.getByRole('button', { name: '交付下载' }).click()
    await adminPage.waitForURL(/#\/deliveries\/[0-9a-f-]+/, { timeout: 30_000 })
    const deliveryID = adminPage.url().match(/#\/deliveries\/([0-9a-f-]+)/)?.[1]
    check(Boolean(deliveryID), 'delivery id missing from the admin URL')
    await adminPage.locator('.title-row h1').filter({ hasText: '第 1 轮交付' }).waitFor({ timeout: 30_000 })
    console.log(`delivery-e2e: delivery ${deliveryID} created from the approved round`)

    /* ----- owner builds both packages ----- */
    await adminPage.getByRole('button', { name: '开始构建' }).click()
    const buildDialog = adminPage.getByRole('dialog', { name: '构建交付包' })
    await buildDialog.waitFor({ timeout: 15_000 })
    await buildDialog.getByRole('button', { name: '提交构建' }).click()
    await adminPage.locator('.title-row .pill').filter({ hasText: '可下载' }).waitFor({ timeout: 240_000 })
    const packageCards = adminPage.locator('.package-card')
    await packageCards.first().waitFor({ timeout: 15_000 })
    check((await packageCards.count()) === 2, `admin shows ${await packageCards.count()} packages, want 2`)
    console.log('delivery-e2e: build reached 可下载 with web and original packages')

    /* ----- owner issues a link and emails it ----- */
    const tokensPanel = adminPage.locator('.panel').filter({ hasText: '下载链接' }).first()
    await tokensPanel.getByLabel('备注').fill('delivery-e2e')
    await tokensPanel.getByLabel('下载次数上限（默认 20）').fill('3')
    await tokensPanel.getByRole('button', { name: '签发链接' }).click()
    const issuedDialog = adminPage.getByRole('dialog', { name: '下载链接已签发' })
    await issuedDialog.waitFor({ timeout: 15_000 })
    deliveryURL = await issuedDialog.locator('input').first().inputValue()
    check(deliveryURL.includes('/#/d/'), `delivery link is not fragment based: ${deliveryURL}`)
    check(deliveryURL.startsWith(cfg.gallery), `delivery link points at ${deliveryURL}, expected ${cfg.gallery}`)
    await issuedDialog.getByRole('button', { name: '我已保存' }).click()

    await adminPage.getByRole('button', { name: '发送交付邮件' }).click()
    console.log('delivery-e2e: link issued and the delivery email queued')

    /* ----- customer downloads on a phone-sized viewport ----- */
    const mobile = await browser.newContext({
      viewport: { width: 390, height: 844 },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 2,
      acceptDownloads: true,
    })
    customerPage = await mobile.newPage()
    watchPage(customerPage, 'customer', errors)
    await customerPage.goto(deliveryURL, { waitUntil: 'domcontentloaded' })
    await customerPage.locator('.album-title').filter({ hasText: fixture.title }).waitFor({ timeout: 30_000 })
    check(!customerPage.url().includes('#/d/') || customerPage.url().endsWith('#/d'), `fragment token was not stripped: ${customerPage.url()}`)
    const quota = customerPage.locator('.delivery-quota')
    await quota.filter({ hasText: '剩余 3 次下载' }).waitFor({ timeout: 15_000 })
    const customerPackages = customerPage.locator('.package-card')
    await customerPackages.first().waitFor({ timeout: 15_000 })
    check((await customerPackages.count()) === 2, `customer sees ${await customerPackages.count()} packages, want 2`)

    const [webDownload] = await Promise.all([
      customerPage.waitForEvent('download', { timeout: 60_000 }),
      customerPage.getByRole('button', { name: '下载网页版包' }).click(),
    ])
    check(webDownload.suggestedFilename().endsWith('.zip'), `web package filename ${webDownload.suggestedFilename()}`)
    const webBytes = readFileSync(await webDownload.path())
    check(webBytes.subarray(0, 2).toString('latin1') === 'PK', 'web package is not a ZIP')
    check(webBytes.includes(Buffer.from('manifest.json')), 'web package has no manifest.json entry')
    await quota.filter({ hasText: '剩余 2 次下载' }).waitFor({ timeout: 15_000 })
    console.log(`delivery-e2e: customer downloaded ${webDownload.suggestedFilename()} (${webBytes.length} bytes)`)

    const firstCard = customerPage.locator('.file-card').first()
    const [originalDownload] = await Promise.all([
      customerPage.waitForEvent('download', { timeout: 60_000 }),
      firstCard.getByRole('button', { name: '原图', exact: true }).click(),
    ])
    const originalBytes = readFileSync(await originalDownload.path())
    check(
      createHash('sha256').update(originalBytes).digest('hex') === createHash('sha256').update(fixture.images[0]).digest('hex'),
      'downloaded original does not match the uploaded bytes',
    )
    await quota.filter({ hasText: '剩余 1 次下载' }).waitFor({ timeout: 15_000 })
    console.log(`delivery-e2e: customer downloaded the original ${originalDownload.suggestedFilename()}`)

    /* ----- revoking the link ends the customer session ----- */
    // Notify issued a second token whose recipient is the customer email, so
    // target the row by its exact label instead of any text containing it.
    await adminPage.bringToFront()
    await adminPage.getByRole('button', { name: '刷新' }).first().click()
    const customerRow = tokensPanel
      .locator('tbody tr')
      .filter({ has: adminPage.getByText('delivery-e2e', { exact: true }) })
    check((await customerRow.count()) === 1, `labelled token rows: ${await customerRow.count()}`)
    await customerRow.getByRole('button', { name: '撤销', exact: true }).click()
    const revokeDialog = adminPage.getByRole('dialog', { name: '撤销下载链接' })
    await revokeDialog.waitFor({ timeout: 15_000 })
    await revokeDialog.getByRole('button', { name: '撤销链接' }).click()
    await customerRow.getByText('已撤销').waitFor({ timeout: 30_000 })

    await customerPage.bringToFront()
    await customerPage.locator('.header-actions').getByRole('button', { name: '刷新' }).click()
    await customerPage.getByRole('heading', { name: '下载链接已停用' }).waitFor({ timeout: 30_000 })
    console.log('delivery-e2e: revoked link is rejected on the next customer request')

    // The refresh after revocation is answered with 410 by design; Chromium
    // logs every failed response as a console error, so only that expected
    // line is tolerated. Page errors and any other failed request still fail.
    const unexpected = errors.filter(
      (line) => !/console: Failed to load resource: the server responded with a status of 410/.test(line),
    )
    check(unexpected.length === 0, `browser reported errors:\n- ${unexpected.join('\n- ')}`)
  } catch (err) {
    if (adminPage) await shot(adminPage, 'delivery-admin-failure')
    if (customerPage) await shot(customerPage, 'delivery-customer-failure')
    if (errors.length) console.error(`browser errors before failure:\n- ${errors.join('\n- ')}`)
    throw err
  } finally {
    await browser.close()
  }

  await waitDeliveryMail()
  console.log('delivery-e2e: OK')
}

run().catch((err) => {
  console.error(`delivery-e2e: FAILED: ${err.stack || err.message}`)
  process.exit(1)
})
