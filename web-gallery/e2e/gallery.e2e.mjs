// End-to-end walkthrough of the M5 gallery flow through the real UIs:
// owner signs in, creates a gallery from uploaded assets, hits the derivative
// publish gate, backfills previews, publishes, issues a share link; a customer
// opens the link, selects and rejects photos, inspects the watermarked display,
// submits; the owner sees the result. Fixture assets are seeded through the
// public API so the browser test starts from a realistic project.
import { chromium } from 'playwright'
import { createHash } from 'node:crypto'
import { deflateSync } from 'node:zlib'

const cfg = {
  api: (process.env.PWF_E2E_API ?? 'http://127.0.0.1:18084').replace(/\/+$/, ''),
  admin: (process.env.PWF_E2E_ADMIN ?? 'http://127.0.0.1:4175').replace(/\/+$/, ''),
  gallery: (process.env.PWF_E2E_GALLERY ?? 'http://127.0.0.1:4174').replace(/\/+$/, ''),
  email: process.env.PWF_E2E_EMAIL ?? 'owner@example.com',
  password: process.env.PWF_E2E_PASSWORD ?? 'photographer123',
  photos: clamp(Number(process.env.PWF_E2E_PHOTOS ?? '4'), 2, 20),
  shots: process.env.PWF_E2E_SHOTS ?? '',
}

function clamp(value, min, max) {
  if (!Number.isFinite(value)) return min
  return Math.min(max, Math.max(min, Math.trunc(value)))
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

// A small, decodable PNG so the server's fallback derivation (no agent
// preview) has real pixels to work with. Each photo differs in hue.
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

async function seedProject(token) {
  const title = `Gallery E2E ${new Date().toISOString().slice(11, 19)}`
  const project = await api('/api/v1/projects', { method: 'POST', token, body: { title } })
  const width = 800
  const height = 600
  const files = []
  const images = []
  for (let i = 0; i < cfg.photos; i++) {
    const bytes = makePNG(width, height, i)
    images.push(bytes)
    files.push({
      rel_path: `e2e/IMG_${String(i + 1).padStart(4, '0')}.PNG`,
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
    body: { project_id: project.id, device_id: 'gallery-e2e' },
  })
  const registered = await api(`/agent/v1/ingest-batches/${batch.id}/assets/batch`, {
    method: 'POST',
    token,
    body: { files },
  })
  check(registered.accepted === cfg.photos, `registered ${registered.accepted} files, want ${cfg.photos}`)
  for (let i = 0; i < registered.assets.length; i++) {
    const assetID = registered.assets[i].asset_id
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

  const status = await until(
    'ingest verification',
    async () => {
      const view = await api(`/agent/v1/ingest-batches/${batch.id}`, { token })
      if (view.verify_job?.status === 'failed') throw new Error(`verify job failed: ${view.verify_job.last_error}`)
      if (view.assets?.verified === cfg.photos && view.assets?.missing === 0) return view
      return null
    },
    { timeout: 120_000 },
  )
  console.log(`gallery-e2e: seeded project ${project.id} with ${cfg.photos} verified PNGs (${status.assets.verified})`)
  return { projectID: project.id, title }
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
    // Navigations that the test itself aborts are not failures; everything
    // else, especially image/API requests, must succeed.
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

async function run() {
  console.log(`gallery-e2e: fixture via API ${cfg.api}`)
  const token = (await api('/api/v1/auth/login', {
    method: 'POST',
    body: { email: cfg.email, password: cfg.password },
  })).access_token
  check(typeof token === 'string' && token.length > 0, 'login returned no access token')
  const { title } = await seedProject(token)

  const browser = await launchBrowser()
  const errors = []
  let adminPage
  let customerPage
  try {
    /* ----- staff console ----- */
    const admin = await browser.newContext({ viewport: { width: 1440, height: 960 } })
    adminPage = await admin.newPage()
    watchPage(adminPage, 'admin', errors)
    await adminPage.goto(cfg.admin + '/', { waitUntil: 'domcontentloaded' })
    await adminPage.getByLabel('邮箱').fill(cfg.email)
    await adminPage.getByLabel('密码').fill(cfg.password)
    await adminPage.getByRole('button', { name: '登录' }).click()
    await adminPage.getByRole('heading', { name: '选片相册' }).waitFor({ timeout: 30_000 })
    console.log('gallery-e2e: admin signed in')

    await adminPage.getByRole('button', { name: '新建相册' }).click()
    const create = adminPage.getByRole('dialog', { name: '新建相册' })
    await create.waitFor()
    await create.getByLabel(/相册标题/).fill(title)
    await create.getByLabel(/选片上限/).fill('3')
    await create.getByLabel(/水印文字/).fill('PWF E2E')
    await create.getByRole('button', { name: '创建相册' }).click()
    await adminPage.waitForURL(/#\/g\/[0-9a-f-]+/, { timeout: 30_000 })
    const galleryID = adminPage.url().match(/#\/g\/([0-9a-f-]+)/)?.[1]
    check(Boolean(galleryID), 'gallery id missing from the URL')
    await adminPage.getByRole('heading', { name: title, exact: true }).waitFor({ timeout: 30_000 })
    console.log(`gallery-e2e: created gallery ${galleryID}`)

    const addable = adminPage.locator('.asset-card:not(.attached)')
    await addable.first().waitFor({ timeout: 30_000 })
    const available = await addable.count()
    check(available >= 2, `asset picker lists ${available} assets, want at least 2`)
    await addable.nth(0).click()
    await addable.nth(1).click()
    await adminPage.getByText(/已选 2 张/).waitFor()
    await adminPage.getByRole('button', { name: /加入相册/ }).click()
    await until('two attached assets', async () => (await adminPage.locator('.asset-card.attached').count()) === 2)
    console.log('gallery-e2e: attached 2 assets')

    // The publish gate must refuse before the worker has rendered previews.
    const gateStart = errors.length
    await adminPage.getByRole('button', { name: '发布相册' }).click()
    const publishDialog = adminPage.getByRole('dialog', { name: '发布相册' })
    await publishDialog.waitFor()
    await publishDialog.getByRole('button', { name: '发布', exact: true }).click()
    await adminPage.locator('.publish-issue').waitFor({ timeout: 30_000 })
    const issueText = await adminPage.locator('.publish-issue').innerText()
    check(issueText.includes('无法发布'), `unexpected publish gate copy: ${issueText}`)
    // The browser logs the deliberate 409 as a console error; drop only that
    // one so any other error still fails the run.
    for (let i = errors.length - 1; i >= gateStart; i--) {
      if (errors[i].includes('409 (Conflict)')) errors.splice(i, 1)
    }
    console.log('gallery-e2e: publish refused while previews were missing')

    await adminPage.locator('.publish-issue').getByRole('button', { name: '补齐预览图' }).click()
    await until(
      'derivatives ready',
      async () => {
        const page = await api(`/api/v1/galleries/${galleryID}/assets?limit=60&offset=0&filter=all`, { token })
        return page.derivatives.missing === 0 && page.derivatives.failed === 0 && page.items.every((item) => item.preview_ready)
      },
      { timeout: 180_000, interval: 1000 },
    )
    await adminPage.locator('.detail-actions').getByRole('button', { name: '刷新' }).click()
    await adminPage.locator('.publish-issue').waitFor({ state: 'hidden', timeout: 15_000 })
    console.log('gallery-e2e: previews derived')

    await adminPage.getByRole('button', { name: '发布相册' }).click()
    await adminPage.getByRole('dialog', { name: '发布相册' }).getByRole('button', { name: '发布', exact: true }).click()
    await adminPage.getByRole('heading', { name: '创建分享链接' }).waitFor({ timeout: 30_000 })
    await adminPage.getByLabel('备注').fill('E2E 手机')
    await adminPage.getByRole('button', { name: '生成链接' }).click()
    const issued = adminPage.getByRole('dialog', { name: '分享链接' })
    await issued.waitFor({ timeout: 30_000 })
    const link = await issued.locator('input').first().inputValue()
    check(link.includes('/#/a/'), `share link is not fragment based: ${link}`)
    check(link.startsWith(cfg.gallery), `share link points at ${link}, expected ${cfg.gallery}`)
    await issued.getByRole('button', { name: '我已保存' }).click()
    console.log(`gallery-e2e: published and issued link ${link.replace(/#\/a\/.*/, '#/a/…')}`)

    /* ----- customer H5 ----- */
    const mobile = await browser.newContext({
      viewport: { width: 390, height: 844 },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 2,
    })
    customerPage = await mobile.newPage()
    watchPage(customerPage, 'customer', errors)
    await customerPage.goto(link, { waitUntil: 'domcontentloaded' })
    await customerPage.locator('.album-title').filter({ hasText: title }).waitFor({ timeout: 30_000 })
    await customerPage.locator('.card img.is-loaded').first().waitFor({ timeout: 60_000 })
    console.log('gallery-e2e: customer opened the album and thumbnails loaded')

    await customerPage.locator('.card-action.select').first().click()
    await until('first photo selected', async () => (await customerPage.locator('.card.is-selected').count()) === 1)
    await customerPage.locator('.card-action.reject').nth(1).click()
    await until('second photo rejected', async () => (await customerPage.locator('.card.is-rejected').count()) === 1)

    await customerPage.locator('.card-open').first().click()
    const lightbox = customerPage.getByRole('dialog', { name: '照片查看' })
    await lightbox.waitFor({ timeout: 15_000 })
    await until(
      'watermarked display preview',
      async () => {
        const image = lightbox.locator('.lb-image')
        const src = await image.getAttribute('src')
        if (!src || !src.includes('display.jpg')) return false
        return image.evaluate((node) => node.complete && node.naturalWidth > 0)
      },
      { timeout: 60_000 },
    )
    await lightbox.getByRole('button', { name: '关闭' }).click()
    console.log('gallery-e2e: lightbox served the watermarked display preview')

    await until('save flush', async () => (await customerPage.getByRole('button', { name: '提交选片' }).isEnabled()))
    await customerPage.getByRole('button', { name: '提交选片' }).click()
    const submitDialog = customerPage.getByRole('dialog', { name: '提交选片' })
    await submitDialog.waitFor()
    await submitDialog.getByRole('button', { name: '确认提交' }).click()
    await customerPage.locator('.sent-banner').waitFor({ timeout: 30_000 })
    console.log('gallery-e2e: customer submitted selections')

    /* ----- staff sees the result ----- */
    await adminPage.bringToFront()
    await adminPage.locator('.detail-actions').getByRole('button', { name: '刷新' }).click()
    await adminPage.getByText('客户已提交').waitFor({ timeout: 15_000 })
    await adminPage.locator('.tab').filter({ hasText: '选片结果' }).click()
    await adminPage.locator('.selection-table tbody tr').first().waitFor({ timeout: 30_000 })
    const selectedPill = await adminPage.locator('.selection-table .pill.ok').count()
    check(selectedPill === 1, `results table shows ${selectedPill} selected rows, want 1`)
    const selectedStat = await adminPage.locator('.stat-strip .stat').nth(1).locator('.stat-value').innerText()
    check(selectedStat.trim() === '1', `selected stat is ${selectedStat}, want 1`)
    console.log('gallery-e2e: admin results reflect the submission')

    check(errors.length === 0, `browser reported errors:\n- ${errors.join('\n- ')}`)
  } catch (err) {
    if (adminPage) await shot(adminPage, 'admin-failure')
    if (customerPage) await shot(customerPage, 'customer-failure')
    if (errors.length) console.error(`browser errors before failure:\n- ${errors.join('\n- ')}`)
    throw err
  } finally {
    await browser.close()
  }
  console.log('gallery-e2e: OK')
}

run().catch((err) => {
  console.error(`gallery-e2e: FAILED: ${err.stack || err.message}`)
  process.exit(1)
})
