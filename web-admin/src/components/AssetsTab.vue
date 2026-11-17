<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { AssetChangeResult, CandidateAsset, CandidatePage, GalleryDetail } from '../api/types'
import {
  completeUpload,
  createIngestBatch,
  enqueueProjectDerivatives,
  finalizeIngestBatch,
  getIngestBatch,
  listProjectAssets,
  presignUpload,
  registerIngestFiles,
  uploadPresignedFile,
  type IngestAsset,
} from '../api/ingest'
import PagerBar from './PagerBar.vue'
import { api, auth } from '../state/session'
import { toast } from '../state/toast'
import { compressImageForUpload, imageSize, sha256Hex } from '../utils/upload'

const props = defineProps<{ gallery: GalleryDetail }>()
const emit = defineEmits<{ changed: [] }>()

const items = ref<CandidateAsset[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 60
const search = ref('')
const loading = ref(false)
const adding = ref(false)
const uploading = ref(false)
const uploadStatus = ref('')
const uploadProgress = ref({ done: 0, total: 0 })
const error = ref('')
const selected = ref<Set<string>>(new Set())
const uploadInput = ref<HTMLInputElement | null>(null)

const editable = computed(() => props.gallery.status === 'draft')
const inGalleryCount = computed(() => items.value.filter((item) => item.in_gallery).length)
const addable = computed(() => items.value.filter((item) => !item.in_gallery))
const allOnPageSelected = computed(
  () => addable.value.length > 0 && addable.value.every((item) => selected.value.has(item.asset_id)),
)
const selectedCount = computed(() => selected.value.size)
const missingPreview = computed(() => items.value.filter((item) => !item.preview_ready).length)
const canUpload = computed(() => editable.value && ['owner', 'staff', 'assistant'].includes(auth.role))

const maxBrowserImageBytes = 50_000_000
const browserImageTypes: Record<string, string> = {
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  png: 'image/png',
  webp: 'image/webp',
}

let debounce = 0
let refetchedThumbs = false

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset.value) })
  if (search.value.trim()) params.set('q', search.value.trim())
  try {
    const page = await api<CandidatePage>(`/api/v1/galleries/${props.gallery.id}/candidates?${params.toString()}`)
    items.value = page.items
    total.value = page.total
    refetchedThumbs = false
    for (const id of [...selected.value]) {
      if (!page.items.some((item) => item.asset_id === id)) selected.value.delete(id)
    }
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载项目资产失败'
  } finally {
    loading.value = false
  }
}

watch(search, () => {
  window.clearTimeout(debounce)
  debounce = window.setTimeout(() => {
    offset.value = 0
    void load()
  }, 280)
})

function toggle(item: CandidateAsset): void {
  if (!editable.value || item.in_gallery) return
  const next = new Set(selected.value)
  if (next.has(item.asset_id)) next.delete(item.asset_id)
  else next.add(item.asset_id)
  selected.value = next
}

function togglePage(): void {
  if (!editable.value) return
  const next = new Set(selected.value)
  if (allOnPageSelected.value) {
    for (const item of addable.value) next.delete(item.asset_id)
  } else {
    for (const item of addable.value) next.add(item.asset_id)
  }
  selected.value = next
}

function clearSelection(): void {
  selected.value = new Set()
}

async function addSelected(): Promise<void> {
  if (adding.value || selected.value.size === 0) return
  adding.value = true
  try {
    const result = await api<AssetChangeResult>(`/api/v1/galleries/${props.gallery.id}/assets`, {
      method: 'POST',
      body: { asset_ids: [...selected.value] },
    })
    toast(`已加入 ${result.added} 张${result.skipped ? `，跳过 ${result.skipped} 张` : ''}`, 'success')
    clearSelection()
    await load()
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '加入相册失败', 'error')
  } finally {
    adding.value = false
  }
}

async function listAllProjectAssets(): Promise<IngestAsset[]> {
  const assets: IngestAsset[] = []
  let offset = 0
  let total = 0
  do {
    const page = await listProjectAssets(props.gallery.project_id, offset)
    assets.push(...page.items)
    total = page.total
    offset += page.items.length
    if (page.items.length === 0) break
  } while (offset < total)
  return assets
}

function mimeType(file: File): string | null {
  const extension = file.name.split('.').pop()?.toLowerCase() ?? ''
  const expected = browserImageTypes[extension]
  if (!expected) return null
  return !file.type || file.type === 'application/octet-stream' ? expected : file.type === expected ? expected : null
}

function assetCanJoinGallery(status: string): boolean {
  return ['verified', 'derivative_ready'].includes(status)
}

async function waitForVerification(batchID: string): Promise<{ completed: boolean; verified: number; missing: number; corrupt: number }> {
  const deadline = Date.now() + 90_000
  while (Date.now() < deadline) {
    const result = await getIngestBatch(batchID)
    if (['completed', 'partial', 'failed'].includes(result.batch.status)) {
      return {
        completed: true,
        verified: result.assets.verified,
        missing: result.assets.missing,
        corrupt: result.assets.corrupt,
      }
    }
    await new Promise((resolve) => window.setTimeout(resolve, 1500))
  }
  return { completed: false, verified: 0, missing: 0, corrupt: 0 }
}

async function uploadSelectedImages(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  if (!files.length || uploading.value) return
  if (!canUpload.value) {
    error.value = '当前账号不能上传项目图片，或该相册已冻结。'
    return
  }
  if (files.length > 100) {
    error.value = '一次最多上传 100 张图片，请分批选择。'
    return
  }
  const prepared = [] as {
    file: File
    hash: string
    mime: string
    width: number
    height: number
    relPath: string
  }[]
  uploading.value = true
  uploadProgress.value = { done: 0, total: files.length }
  error.value = ''
  try {
  for (const [index, file] of files.entries()) {
    const originalMime = mimeType(file)
    if (!originalMime) {
      error.value = `“${file.name}”不是支持的图片格式，请选择 JPEG、PNG 或 WebP。`
      uploading.value = false
      return
    }
    if (file.size <= 0) {
      error.value = `“${file.name}”为空，无法上传。`
      uploading.value = false
      return
    }
    let uploadFile = file
    if (file.size > maxBrowserImageBytes) {
      uploadStatus.value = `正在压缩 ${file.name}（超过 50 MB）…`
      try {
        uploadFile = await compressImageForUpload(file, maxBrowserImageBytes)
      } catch (reason) {
        error.value = reason instanceof Error ? reason.message : `“${file.name}”压缩失败`
        uploading.value = false
        uploadStatus.value = ''
        return
      }
    }
    const mime = mimeType(uploadFile)
    if (!mime || uploadFile.size > maxBrowserImageBytes) {
      error.value = `“${file.name}”压缩后仍超过 50 MB，无法上传。`
      uploading.value = false
      uploadStatus.value = ''
      return
    }
    const { width, height } = await imageSize(uploadFile)
    prepared.push({
      file: uploadFile,
      hash: await sha256Hex(uploadFile),
      mime,
      width,
      height,
      relPath: `browser/${String(index + 1).padStart(3, '0')}-${uploadFile.name.replace(/[\\/:]/g, '_')}`,
    })
  }
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '读取或处理图片失败'
    uploading.value = false
    uploadStatus.value = ''
    return
  }

  uploadProgress.value = { done: 0, total: prepared.length }
  uploadStatus.value = ''
  try {
    uploadStatus.value = '检查项目中已有图片…'
    const existingAssets = await listAllProjectAssets()
    const existingByHash = new Map(existingAssets.map((asset) => [asset.content_hash, asset]))
    const seenHashes = new Set<string>()
    const toImport = [] as typeof prepared
    const alreadyReady = new Set<string>()
    let alreadyPending = 0
    let duplicateSelection = 0
    for (const item of prepared) {
      if (seenHashes.has(item.hash)) {
        duplicateSelection++
        continue
      }
      seenHashes.add(item.hash)
      const existing = existingByHash.get(item.hash)
      if (!existing) {
        toImport.push(item)
      } else if (assetCanJoinGallery(existing.status)) {
        alreadyReady.add(existing.id)
      } else if (['registered', 'missing', 'corrupt'].includes(existing.status)) {
        toImport.push(item)
      } else {
        alreadyPending++
      }
    }

    let newlyRegisteredIDs: string[] = []
    let verification = { completed: true, verified: 0, missing: 0, corrupt: 0 }
    if (toImport.length) {
      uploadStatus.value = '创建图片导入批次…'
      const batch = await createIngestBatch(props.gallery.project_id)
      const registered = await registerIngestFiles(batch.id, toImport.map((item) => ({
        rel_path: item.relPath,
        size: item.file.size,
        xxh64: '',
        sha256: item.hash,
        mime: item.mime,
        width: item.width,
        height: item.height,
        exif: {},
        is_video: false,
      })))
      const itemByPath = new Map(toImport.map((item) => [item.relPath, item]))
      const uploadByID = new Map<string, typeof toImport[number]>()
      for (const asset of registered.assets) {
        const item = itemByPath.get(asset.rel_path)
        if (!item) continue
        newlyRegisteredIDs.push(asset.asset_id)
        if (['registered', 'missing', 'corrupt'].includes(asset.status)) uploadByID.set(asset.asset_id, item)
        else if (assetCanJoinGallery(asset.status)) alreadyReady.add(asset.asset_id)
        else alreadyPending++
      }
      const uniqueUploads = [...uploadByID.entries()]
      uploadProgress.value = { done: 0, total: uniqueUploads.length }
      for (const [assetID, item] of uniqueUploads) {
        uploadStatus.value = `上传图片 ${uploadProgress.value.done + 1}/${uniqueUploads.length}：${item.file.name}`
        const plan = await presignUpload(assetID, item.file.size)
        await uploadPresignedFile(plan, item.file)
        await completeUpload(assetID)
        uploadProgress.value = { done: uploadProgress.value.done + 1, total: uniqueUploads.length }
      }
      uploadStatus.value = '提交图片校验…'
      await finalizeIngestBatch(batch.id)
      uploadStatus.value = '等待服务器校验图片…'
      verification = await waitForVerification(batch.id)
    }

    let readyIDs = new Set(alreadyReady)
    if (newlyRegisteredIDs.length) {
      const uploadedIDs = new Set(newlyRegisteredIDs)
      for (const asset of await listAllProjectAssets()) {
        if (uploadedIDs.has(asset.id) && assetCanJoinGallery(asset.status)) readyIDs.add(asset.id)
      }
    }
    let attached = 0
    if (readyIDs.size) {
      uploadStatus.value = '将图片加入当前相册…'
      const result = await api<AssetChangeResult>(`/api/v1/galleries/${props.gallery.id}/assets`, {
        method: 'POST',
        body: { asset_ids: [...readyIDs] },
      })
      attached = result.added
    }
    if (toImport.length && verification.completed && verification.verified > 0) {
      uploadStatus.value = '排队生成图片预览…'
      await enqueueProjectDerivatives(props.gallery.project_id, [...readyIDs])
    }

    await load()
    emit('changed')
    const parts = [`已加入相册 ${attached} 张`]
    if (!verification.completed) parts.push('其余图片仍在后台校验，请稍后刷新')
    if (verification.missing || verification.corrupt) parts.push(`校验失败 ${verification.missing + verification.corrupt} 张`)
    if (alreadyPending) parts.push(`另有 ${alreadyPending} 张已在处理中`)
    if (duplicateSelection) parts.push(`跳过重复选择 ${duplicateSelection} 张`)
    if (toImport.length === 0 && attached === 0 && alreadyPending === 0) parts.splice(0, 1, '所选图片已在相册中')
    toast(parts.join('；'), verification.missing || verification.corrupt ? 'error' : 'success')
  } catch (reason) {
    error.value = reason instanceof ApiError ? reason.detail || reason.message : reason instanceof Error ? reason.message : '图片上传失败'
  } finally {
    uploading.value = false
    uploadStatus.value = ''
  }
}

function onThumbError(): void {
  // Presigned URLs expire after ~15 minutes; one throttled refresh re-signs the
  // page without hammering the API for every broken image.
  if (refetchedThumbs || loading.value) return
  refetchedThumbs = true
  void load()
}

onMounted(load)
</script>

<template>
  <section class="tab-panel">
    <div class="toolbar">
      <label class="inline-field grow">
        <span>搜索</span>
        <input v-model="search" type="search" placeholder="按文件名搜索项目资产" />
      </label>
      <div class="toolbar-note muted small">
        本页 {{ items.length }} 张 · 已加入相册 {{ inGalleryCount }} 张<template v-if="missingPreview"> ·
          {{ missingPreview }} 张缺少缩略图</template>
      </div>
      <input
        ref="uploadInput"
        class="upload-input"
        type="file"
        accept="image/jpeg,image/png,image/webp,.jpg,.jpeg,.png,.webp"
        multiple
        @change="uploadSelectedImages"
      />
      <button class="btn primary sm" type="button" :disabled="uploading || !canUpload" @click="uploadInput?.click()">
        {{ uploading ? '上传中…' : '上传图片' }}
      </button>
      <button class="btn ghost sm" type="button" :disabled="loading" @click="load">刷新</button>
    </div>

    <p class="muted small upload-hint">选择本地 JPEG、PNG 或 WebP 图片（单张不超过 50 MB）；更大的图片会先在浏览器压缩到 50 MB 以内。</p>
    <p v-if="uploading" class="upload-progress" role="status">
      <span>{{ uploadStatus }}</span>
      <span v-if="uploadProgress.total">{{ uploadProgress.done }}/{{ uploadProgress.total }}</span>
    </p>
    <p v-if="!editable" class="notice">
      相册已发布，资产集合已冻结。如需调整内容，请新建下一轮相册。
    </p>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="action-bar" v-if="editable">
      <label class="checkbox-inline">
        <input type="checkbox" :checked="allOnPageSelected" :disabled="addable.length === 0" @change="togglePage" />
        全选本页未加入项
      </label>
      <span class="muted small" v-if="selectedCount">已选 {{ selectedCount }} 张</span>
      <div class="spacer"></div>
      <button v-if="selectedCount" class="btn ghost sm" type="button" @click="clearSelection">清除</button>
      <button class="btn primary sm" type="button" :disabled="adding || selectedCount === 0" @click="addSelected">
        {{ adding ? '加入中…' : `加入相册${selectedCount ? `（${selectedCount}）` : ''}` }}
      </button>
    </div>

    <div class="asset-grid">
      <button
        v-for="item in items"
        :key="item.asset_id"
        type="button"
        class="asset-card"
        :class="{
          selected: selected.has(item.asset_id),
          attached: item.in_gallery,
          disabled: !editable && !item.in_gallery,
        }"
        :title="item.in_gallery ? '已在相册中' : item.preview_ready ? '' : '预览图尚未生成'"
        @click="toggle(item)"
      >
        <span class="asset-thumb" :style="item.color && !item.preview_ready ? { background: item.color } : undefined">
          <img
            v-if="item.thumb_url"
            :src="item.thumb_url"
            :alt="item.filename"
            loading="lazy"
            decoding="async"
            @error="onThumbError"
          />
          <span v-else-if="item.lqip" class="lqip" :style="{ backgroundImage: `url(${item.lqip})` }"></span>
          <span v-else class="thumb-empty muted">无预览</span>
          <span v-if="item.in_gallery" class="attached-badge">已加入</span>
          <span v-else-if="selected.has(item.asset_id)" class="selected-badge">✓</span>
          <span v-if="item.is_video" class="video-badge">视频</span>
        </span>
        <span class="asset-meta">
          <span class="asset-name">{{ item.filename }}</span>
          <span class="muted small">{{ item.width }}×{{ item.height }}</span>
        </span>
      </button>
      <p v-if="!loading && items.length === 0" class="empty muted">
        {{ search ? '没有匹配的资产' : '项目还没有可加入的资产（需要已完成校验）' }}
      </p>
    </div>

    <p v-if="loading" class="empty muted">加载中…</p>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; load() }"
    />
  </section>
</template>

<style scoped>
.upload-input { display: none; }
.upload-hint { margin: -8px 0 14px; }
.upload-progress { display: flex; justify-content: space-between; gap: 16px; padding: 9px 12px; margin: 0 0 12px; border: 1px solid var(--border); background: var(--surface-subtle, #f6f7f5); color: var(--text); font-size: 13px; }
</style>
