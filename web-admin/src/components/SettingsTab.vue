<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { Gallery, GalleryDetail } from '../api/types'
import ConfirmDialog from './ConfirmDialog.vue'
import { api } from '../state/session'
import { toast } from '../state/toast'
import { formatDateTime, fromLocalInput, toLocalInput } from '../utils/format'
import { parseNumberInput } from '../utils/number'

const props = defineProps<{ gallery: GalleryDetail }>()
const emit = defineEmits<{ changed: [] }>()

const title = ref('')
const expiresAt = ref('')
const clearExpires = ref(false)
const limit = ref('')
const clearLimit = ref(false)
const watermark = ref('')
const download = ref(false)
const busy = ref(false)
const error = ref('')
const actionBusy = ref(false)
const revoking = ref(false)
const reopening = ref(false)

const published = computed(() => props.gallery.status === 'published')
const canRevoke = computed(() => props.gallery.status === 'published')
const canReopen = computed(
  () =>
    props.gallery.status === 'published' &&
    Boolean(props.gallery.submitted_at) &&
    (!props.gallery.expires_at || Date.parse(props.gallery.expires_at) > Date.now()),
)

function reset(): void {
  title.value = props.gallery.title
  expiresAt.value = toLocalInput(props.gallery.expires_at)
  clearExpires.value = !props.gallery.expires_at
  limit.value = props.gallery.selection_limit === null ? '' : String(props.gallery.selection_limit)
  clearLimit.value = props.gallery.selection_limit === null
  watermark.value = props.gallery.watermark_text
  download.value = props.gallery.download_allowed
  error.value = ''
}

watch(() => props.gallery.version, reset, { immediate: true })

// v-model on type="number" yields a number, so normalize before trimming.
function limitValue(): number | null {
  if (clearLimit.value) return null
  const parsed = parseNumberInput(limit.value)
  return parsed.empty ? null : parsed.value
}

const dirty = computed(() => {
  const currentTitle = props.gallery.title
  const expiresValue = clearExpires.value ? null : fromLocalInput(expiresAt.value)
  const currentExpires = props.gallery.expires_at ? new Date(props.gallery.expires_at).toISOString() : null
  const next = limitValue()
  return (
    title.value.trim() !== currentTitle ||
    expiresValue !== currentExpires ||
    next !== props.gallery.selection_limit ||
    download.value !== props.gallery.download_allowed ||
    (!published.value && watermark.value.trim() !== props.gallery.watermark_text)
  )
})

async function save(): Promise<void> {
  if (busy.value) return
  if (!title.value.trim()) {
    error.value = '标题不能为空'
    return
  }
  const nextLimit = limitValue()
  if (nextLimit !== null && (!Number.isInteger(nextLimit) || nextLimit <= 0)) {
    error.value = '选片上限必须是正整数，或勾选不限'
    return
  }
  busy.value = true
  error.value = ''
  try {
    await api<Gallery>(`/api/v1/galleries/${props.gallery.id}`, {
      method: 'PATCH',
      body: {
        expected_version: props.gallery.version,
        title: title.value.trim(),
        expires_at: clearExpires.value ? null : fromLocalInput(expiresAt.value),
        selection_limit: nextLimit,
        download_allowed: download.value,
        ...(published.value ? {} : { watermark_text: watermark.value.trim() }),
      },
    })
    toast('设置已保存', 'success')
    emit('changed')
  } catch (err) {
    if (err instanceof ApiError && err.code === 'version_conflict') {
      toast('相册已在别处被修改，已刷新到最新状态', 'error')
      emit('changed')
    } else {
      error.value = err instanceof ApiError ? err.detail || err.message : '保存失败'
    }
  } finally {
    busy.value = false
  }
}

async function revokeGallery(): Promise<void> {
  actionBusy.value = true
  try {
    await api<Gallery>(`/api/v1/galleries/${props.gallery.id}/revoke`, { method: 'POST', body: {} })
    toast('相册已撤销，所有分享链接立即失效', 'success')
    revoking.value = false
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '撤销失败', 'error')
  } finally {
    actionBusy.value = false
  }
}

async function reopenGallery(): Promise<void> {
  actionBusy.value = true
  try {
    await api<Gallery>(`/api/v1/galleries/${props.gallery.id}/reopen`, { method: 'POST', body: {} })
    toast('已重新开放选片，客户可以继续修改', 'success')
    reopening.value = false
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '重新开放失败', 'error')
  } finally {
    actionBusy.value = false
  }
}
</script>

<template>
  <section class="tab-panel settings">
    <form class="panel" @submit.prevent="save">
      <h2 class="panel-title">相册设置</h2>
      <div class="form-grid">
        <label class="field span-2">
          <span>相册标题（客户可见）</span>
          <input v-model="title" type="text" maxlength="120" :disabled="busy" />
        </label>

        <div class="field">
          <span>选片截止时间</span>
          <input v-model="expiresAt" type="datetime-local" :disabled="busy || clearExpires" />
          <label class="checkbox-inline">
            <input v-model="clearExpires" type="checkbox" :disabled="busy" />
            不设截止时间
          </label>
        </div>

        <div class="field">
          <span>选片上限（张）</span>
          <input v-model="limit" type="number" min="1" step="1" :disabled="busy || clearLimit" placeholder="不限" />
          <label class="checkbox-inline">
            <input v-model="clearLimit" type="checkbox" :disabled="busy" />
            不限张数
          </label>
        </div>

        <label class="field">
          <span>水印文字</span>
          <input
            v-model="watermark"
            type="text"
            maxlength="120"
            :disabled="busy || published"
            :placeholder="gallery.watermark_text || '工作室名称'"
          />
          <span v-if="published" class="muted small">已发布的相册不再重建水印图；修改水印需要新建下一轮。</span>
          <span v-else class="muted small">留空使用工作室名称作为水印。</span>
        </label>

        <label class="field checkbox-field">
          <input v-model="download" type="checkbox" :disabled="busy" />
          <span>允许下载（下载入口在交付阶段开放）</span>
        </label>

        <p v-if="error" class="form-error span-2" role="alert">{{ error }}</p>

        <div class="form-actions span-2">
          <button class="btn ghost" type="button" :disabled="busy || !dirty" @click="reset">还原</button>
          <button class="btn primary" type="submit" :disabled="busy || !dirty">
            {{ busy ? '保存中…' : '保存设置' }}
          </button>
        </div>
      </div>
    </form>

    <div class="panel">
      <h2 class="panel-title">相册信息</h2>
      <dl class="meta-list">
        <div><dt>状态</dt><dd>{{ gallery.status }}</dd></div>
        <div><dt>轮次</dt><dd>第 {{ gallery.round_no }} 轮</dd></div>
        <div><dt>清单版本</dt><dd>v{{ gallery.manifest_version }} · 数据版本 {{ gallery.version }}</dd></div>
        <div><dt>选片版本</dt><dd>{{ gallery.selection_version }}</dd></div>
        <div><dt>发布时间</dt><dd>{{ formatDateTime(gallery.published_at) }}</dd></div>
        <div><dt>提交时间</dt><dd>{{ formatDateTime(gallery.submitted_at) }}</dd></div>
        <div><dt>创建时间</dt><dd>{{ formatDateTime(gallery.created_at) }}</dd></div>
        <div><dt>清单对象</dt><dd class="mono">{{ gallery.manifest_key || '—' }}</dd></div>
      </dl>
    </div>

    <div class="panel danger-panel">
      <h2 class="panel-title">状态操作</h2>
      <div class="danger-row">
        <div>
          <strong>重新开放选片</strong>
          <p class="muted small">清空提交状态，客户可以在截止前继续修改选片。需先有提交记录。</p>
        </div>
        <button class="btn sm" type="button" :disabled="!canReopen || actionBusy" @click="reopening = true">
          重新开放
        </button>
      </div>
      <div class="danger-row">
        <div>
          <strong>撤销相册</strong>
          <p class="muted small">立即停止所有分享链接，客户无法再访问。撤销后不可恢复。</p>
        </div>
        <button class="btn danger sm" type="button" :disabled="!canRevoke || actionBusy" @click="revoking = true">
          撤销相册
        </button>
      </div>
    </div>

    <ConfirmDialog
      :open="reopening"
      title="重新开放选片"
      message="客户将可以继续修改并重新提交选片。确认重新开放？"
      confirm-text="重新开放"
      :busy="actionBusy"
      @cancel="reopening = false"
      @confirm="reopenGallery"
    />
    <ConfirmDialog
      :open="revoking"
      title="撤销相册"
      message="所有分享链接将在下一次请求时立即失效，客户无法再打开相册。撤销后不可恢复。"
      confirm-text="确认撤销"
      tone="danger"
      :busy="actionBusy"
      @cancel="revoking = false"
      @confirm="revokeGallery"
    />
  </section>
</template>
