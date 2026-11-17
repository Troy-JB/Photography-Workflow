// End-to-end walkthrough of the M6 proof flow through the real UIs: the owner
// signs in, starts a proof round from a submitted selection and copies the
// fragment link; a customer annotates a photo and requests changes; the owner
// accepts the retouch task, uploads two revisions and resubmits; the customer
// reviews the retouched previews and approves. Fixtures are seeded through the
// public API so the walkthrough starts from a realistic submitted selection.
import { chromium } from 'playwright'
import { createHash } from 'node:crypto'
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

/* ---------- API + fixtures ---------- */

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

// A small decodable PNG so the server has real pixels to derive previews and
// revisions from. Each image differs in hue and brightness.
function makePNG(width, height, index, shift = 0) {
  const raw = Buffer.alloc((width * 3 + 1) * height)
  for (let y = 0; y < height; y++) {
    const row = y * (width * 3 + 1)
    raw[row] = 0
    for (let x = 0; x < width; x++) {
      const offset = row + 1 + x * 3
      raw[offset] = (index * 61 + shift + Math.floor((x * 255) / width)) & 0xff
      raw[offset + 1] = (90 + shift + Math.floor((y * 165) / height)) & 0xff
      raw[offset + 2] = (200 - index * 40 - shift) & 0xff
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

async function seedSelection(token) {
  const stamp = new Date().toISOString().slice(11, 19)
  const title = `Proof E2E ${stamp}`
  const client = await api('/api/v1/clients', {
    method: 'POST',
    token,
    body: { name: 'Proof E2E Customer', email: `proof-e2e+${Date.now()}@example.com` },
  })
  const project = await api('/api/v1/projects', {
    method: 'POST',
    token,
    body: { title, client_id: client.id },
  })
  const width = 800
  const height = 600
  const files = []
  const images = []
  for (let i = 0; i < 2; i++) {
    const bytes = makePNG(width, height, i)
    images.push(bytes)
    files.push({
      rel_path: `proof-e2e/IMG_${String(i + 1).padStart(4, '0')}.PNG`,
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
    body: { project_id: project.id, device_id: 'proof-e2e' },
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
      watermark_text: 'PWF E2E',
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
  // The project derive above renders the default watermark. A gallery with its
  // own watermark_text is re-rendered by the publish gate itself: the first
  // attempt answers 409 derivatives_incomplete after queueing the refresh.
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
    body: { label: 'proof-e2e' },
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
  console.log(`proof-e2e: seeded project ${project.id} with a submitted selection`)
  return { projectID: project.id, galleryID: gallery.id, title, assetIDs }
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

async function waitProofMail() {
  if (!cfg.mailDir) {
    console.log('proof-e2e: mail check skipped (pass PWF_E2E_MAIL_DIR to inspect SMTP)')
    return
  }
  await until(
    'proof email in the SMTP sink',
    async () => capturedBodies().some((body) => body.includes('/#/p/')),
    { timeout: 120_000, interval: 500 },
  )
  console.log('proof-e2e: customer email captured with a fragment proof link')
}

/* ---------- walkthrough ---------- */

async function run() {
  console.log(`proof-e2e: fixture via API ${cfg.api}`)
  const token = (await api('/api/v1/auth/login', {
    method: 'POST',
    body: { email: cfg.email, password: cfg.password },
  })).access_token
  check(typeof token === 'string' && token.length > 0, 'login returned no access token')
  const { galleryID, title, assetIDs } = await seedSelection(token)

  const browser = await launchBrowser()
  const errors = []
  let adminPage
  let customerPage
  try {
    /* ----- owner starts the proof round ----- */
    const admin = await browser.newContext({ viewport: { width: 1440, height: 960 } })
    adminPage = await admin.newPage()
    watchPage(adminPage, 'admin', errors)
    await adminPage.goto(cfg.admin + '/', { waitUntil: 'domcontentloaded' })
    await adminPage.getByLabel('邮箱').fill(cfg.email)
    await adminPage.getByLabel('密码').fill(cfg.password)
    await adminPage.getByRole('button', { name: '登录' }).click()
    await adminPage.getByRole('heading', { name: '选片相册' }).waitFor({ timeout: 30_000 })

    await adminPage.goto(`${cfg.admin}/#/g/${galleryID}`, { waitUntil: 'domcontentloaded' })
    await adminPage.getByRole('heading', { name: title, exact: true }).waitFor({ timeout: 30_000 })
    await adminPage.getByRole('button', { name: '发起审片' }).click()
    const createDialog = adminPage.getByRole('dialog', { name: '发起审片' })
    await createDialog.waitFor({ timeout: 15_000 })
    await createDialog.getByRole('button', { name: '创建审片并发邀请邮件' }).click()
    await adminPage.getByRole('heading', { name: '审片轮次已创建' }).waitFor({ timeout: 30_000 })
    const proofLink = await createDialog.locator('input').first().inputValue()
    check(proofLink.includes('/#/p/'), `proof link is not fragment based: ${proofLink}`)
    check(proofLink.startsWith(cfg.gallery), `proof link points at ${proofLink}, expected ${cfg.gallery}`)
    await createDialog.getByRole('button', { name: '我已保存，进入审片' }).click()
    await adminPage.waitForURL(/#\/p\/[0-9a-f-]+/, { timeout: 30_000 })
    const roundID = adminPage.url().match(/#\/p\/([0-9a-f-]+)/)?.[1]
    check(Boolean(roundID), 'round id missing from the URL')
    await adminPage.getByRole('heading', { name: '第 1 轮审片' }).waitFor({ timeout: 30_000 })
    console.log(`proof-e2e: round ${roundID} created and linked`)

    /* ----- customer annotates and requests changes ----- */
    const mobile = await browser.newContext({
      viewport: { width: 390, height: 844 },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 2,
    })
    customerPage = await mobile.newPage()
    watchPage(customerPage, 'customer', errors)
    await customerPage.goto(proofLink, { waitUntil: 'domcontentloaded' })
    await customerPage.locator('.album-title').filter({ hasText: title }).waitFor({ timeout: 30_000 })
    const cards = customerPage.locator('.card-open')
    await cards.first().waitFor({ timeout: 60_000 })
    const cardCount = await cards.count()
    check(cardCount === 2, `proof grid shows ${cardCount} photos, want 2`)

    await cards.first().click()
    const lightbox = customerPage.getByRole('dialog', { name: '审片查看' })
    await lightbox.waitFor({ timeout: 15_000 })
    await lightbox.locator('.proof-canvas').waitFor({ timeout: 30_000 })
    await lightbox.getByRole('button', { name: '点', exact: true }).click()
    const canvas = lightbox.locator('.proof-canvas')
    const box = await canvas.boundingBox()
    check(box !== null && box.width > 50 && box.height > 50, 'annotation canvas has no layout box')
    await customerPage.mouse.click(box.x + box.width * 0.4, box.y + box.height * 0.45)
    await lightbox.locator('textarea[placeholder="补充说明（可选）"]').fill('请把这一处的天空压暗一点')
    await lightbox.getByRole('button', { name: '发送批注' }).click()
    await lightbox.getByText('请把这一处的天空压暗一点').waitFor({ timeout: 30_000 })
    await lightbox.getByRole('button', { name: '关闭' }).click()
    await lightbox.waitFor({ state: 'hidden', timeout: 15_000 })
    console.log('proof-e2e: customer saved a point annotation with a note')

    await customerPage.getByRole('button', { name: '提出修改' }).click()
    const changesDialog = customerPage.getByRole('dialog', { name: '提出修改' })
    await changesDialog.waitFor()
    await changesDialog.getByRole('button', { name: '提交修改' }).click()
    await customerPage.locator('.proof-status').filter({ hasText: '已提出修改' }).waitFor({ timeout: 30_000 })
    console.log('proof-e2e: customer requested changes')

    /* ----- owner accepts the retouch and uploads revisions ----- */
    await adminPage.bringToFront()
    await adminPage.locator('.detail-actions').getByRole('button', { name: '刷新' }).click()
    await adminPage.locator('.pill').filter({ hasText: '客户提出修改' }).first().waitFor({ timeout: 30_000 })
    await adminPage.locator('.tab').filter({ hasText: '修图' }).click()
    await adminPage.getByRole('button', { name: '接单开始修图' }).click()
    await adminPage.getByText('已导入本轮版本 0 / 2 张').waitFor({ timeout: 30_000 })

    const revisions = [0, 1].map((i) => makePNG(800, 600, i, 36))
    await adminPage.locator('input[type="file"][multiple]').setInputFiles(
      revisions.map((buffer, i) => ({
        name: `IMG_${String(i + 1).padStart(4, '0')}.PNG`,
        mimeType: 'image/png',
        buffer,
      })),
    )
    await adminPage.getByText(/导入 2 个/).first().waitFor({ timeout: 30_000 })
    await until(
      'revision derivatives',
      async () => {
        const detail = await api(`/api/v1/proof-rounds/${roundID}`, { token })
        return detail.items.every((item) => item.revision_no === 1 && item.revision_state === 'derivative_ready')
      },
      { timeout: 180_000, interval: 1000 },
    )
    await adminPage.locator('.detail-actions').getByRole('button', { name: '刷新' }).click()
    await adminPage.getByText('已导入本轮版本 2 / 2 张').waitFor({ timeout: 30_000 })
    await adminPage.getByRole('button', { name: '回传客户确认' }).click()
    const resubmitDialog = adminPage.getByRole('dialog', { name: '回传客户确认' })
    await resubmitDialog.waitFor()
    await resubmitDialog.getByRole('button', { name: '确认回传' }).click()
    await adminPage.locator('.pill').filter({ hasText: '已回传待确认' }).first().waitFor({ timeout: 30_000 })
    console.log('proof-e2e: owner uploaded two revisions and resubmitted')

    /* ----- customer reviews the revisions and approves ----- */
    await customerPage.bringToFront()
    await customerPage.locator('.header-actions').getByRole('button', { name: '刷新' }).click()
    await customerPage.locator('.proof-status').filter({ hasText: '待复核' }).waitFor({ timeout: 30_000 })
    await customerPage.locator('.card-open').first().click()
    await customerPage.getByRole('dialog', { name: '审片查看' }).waitFor({ timeout: 15_000 })
    await customerPage.locator('.proof-rev-chip').filter({ hasText: '修图版 v1' }).waitFor({ timeout: 30_000 })
    await customerPage.getByRole('button', { name: '关闭' }).click()

    await customerPage.getByRole('button', { name: '确认通过' }).click()
    const approveDialog = customerPage.getByRole('dialog', { name: '确认通过' })
    await approveDialog.waitFor()
    await approveDialog.getByRole('button', { name: '确认通过' }).click()
    await customerPage.getByRole('heading', { name: '本轮审片已通过' }).waitFor({ timeout: 30_000 })
    console.log('proof-e2e: customer approved the retouched round')

    /* ----- owner sees the approval ----- */
    await adminPage.bringToFront()
    await adminPage.locator('.detail-actions').getByRole('button', { name: '刷新' }).click()
    await adminPage.locator('.pill').filter({ hasText: '已通过' }).first().waitFor({ timeout: 30_000 })
    console.log('proof-e2e: owner console reflects the approval')

    check(errors.length === 0, `browser reported errors:\n- ${errors.join('\n- ')}`)
  } catch (err) {
    if (adminPage) await shot(adminPage, 'proof-admin-failure')
    if (customerPage) await shot(customerPage, 'proof-customer-failure')
    if (errors.length) console.error(`browser errors before failure:\n- ${errors.join('\n- ')}`)
    throw err
  } finally {
    await browser.close()
  }

  await waitProofMail()
  check(assetIDs.length === 2, 'fixture asset list is incomplete')
  console.log('proof-e2e: OK')
}

run().catch((err) => {
  console.error(`proof-e2e: FAILED: ${err.stack || err.message}`)
  process.exit(1)
})
