<script setup lang="ts">
import { computed, ref } from 'vue'
import { ApiError } from '../api/client'
import type { GalleryDetail, IssuedToken, Token } from '../api/types'
import ConfirmDialog from './ConfirmDialog.vue'
import { api } from '../state/session'
import { toast } from '../state/toast'
import { copyText } from '../utils/clipboard'
import { formatDateTime, fromLocalInput } from '../utils/format'
import { parseNumberInput } from '../utils/number'

const props = defineProps<{ gallery: GalleryDetail }>()
const emit = defineEmits<{ changed: [] }>()

const label = ref('')
const maxVisits = ref('')
const expiresAt = ref('')
const busy = ref(false)
const error = ref('')
const issued = ref<IssuedToken | null>(null)
const revoking = ref<Token | null>(null)
const revokeBusy = ref(false)

const canIssue = computed(() => props.gallery.status === 'published' && !busy.value)

function tokenState(token: Token): { label: string; tone: string } {
  if (token.revoked_at) return { label: '已撤销', tone: 'warn' }
  if (token.expires_at && Date.parse(token.expires_at) <= Date.now()) return { label: '已过期', tone: 'muted' }
  return { label: '有效', tone: 'ok' }
}

async function createToken(): Promise<void> {
  if (!canIssue.value) return
  // v-model on type="number" yields a number, so normalize before trimming.
  const parsedVisits = parseNumberInput(maxVisits.value)
  const visits = parsedVisits.empty ? 0 : parsedVisits.value
  if (!Number.isInteger(visits) || visits < 0) {
    error.value = '访问次数上限必须是非负整数，0 表示不限'
    return
  }
  busy.value = true
  error.value = ''
  try {
    const token = await api<IssuedToken>(`/api/v1/galleries/${props.gallery.id}/tokens`, {
      method: 'POST',
      body: {
        label: label.value.trim(),
        max_visits: visits,
        expires_at: fromLocalInput(expiresAt.value),
      },
    })
    issued.value = token
    label.value = ''
    maxVisits.value = ''
    expiresAt.value = ''
    emit('changed')
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '创建链接失败'
  } finally {
    busy.value = false
  }
}

async function copy(value: string, what: string): Promise<void> {
  const ok = await copyText(value)
  toast(ok ? `${what}已复制` : '复制失败，请手动选择文本复制', ok ? 'success' : 'error')
}

async function confirmRevoke(): Promise<void> {
  const token = revoking.value
  if (!token) return
  revokeBusy.value = true
  try {
    await api<void>(`/api/v1/galleries/${props.gallery.id}/tokens/${token.id}`, { method: 'DELETE' })
    toast('链接已撤销，客户端会在下一次请求时失效', 'success')
    revoking.value = null
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '撤销失败', 'error')
  } finally {
    revokeBusy.value = false
  }
}
</script>

<template>
  <section class="tab-panel">
    <p v-if="gallery.status !== 'published'" class="notice">
      发布相册后才能创建分享链接。{{ gallery.status === 'draft' ? '请先在顶部发布相册。' : '当前相册不可访问。' }}
    </p>

    <div class="panel">
      <h2 class="panel-title">创建分享链接</h2>
      <form class="token-form" @submit.prevent="createToken">
        <label class="field">
          <span>备注</span>
          <input v-model="label" type="text" maxlength="80" placeholder="例如：新娘手机" :disabled="!canIssue" />
        </label>
        <label class="field">
          <span>访问次数上限（0 为不限）</span>
          <input v-model="maxVisits" type="number" min="0" step="1" placeholder="不限" :disabled="!canIssue" />
        </label>
        <label class="field">
          <span>链接过期时间（可选）</span>
          <input v-model="expiresAt" type="datetime-local" :disabled="!canIssue" />
        </label>
        <button class="btn primary" type="submit" :disabled="!canIssue">
          {{ busy ? '创建中…' : '生成链接' }}
        </button>
      </form>
      <p v-if="error" class="form-error" role="alert">{{ error }}</p>
      <p class="muted small token-hint">
        明文链接仅在创建后显示一次，数据库只保存哈希。链接放在 URL fragment 中，不会出现在服务器日志或 Referer 里。
      </p>
    </div>

    <div class="panel">
      <h2 class="panel-title">链接列表</h2>
      <table class="data compact">
        <thead>
          <tr>
            <th>备注</th>
            <th>状态</th>
            <th class="num">访问次数</th>
            <th>过期时间</th>
            <th>最近使用</th>
            <th>创建时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="token in gallery.tokens" :key="token.id">
            <td>{{ token.label || '未命名' }}</td>
            <td><span class="pill" :class="tokenState(token).tone">{{ tokenState(token).label }}</span></td>
            <td class="num">{{ token.visits }}<span class="muted"> / {{ token.max_visits || '∞' }}</span></td>
            <td>{{ formatDateTime(token.expires_at) }}</td>
            <td>{{ formatDateTime(token.last_used_at) }}</td>
            <td>{{ formatDateTime(token.created_at) }}</td>
            <td class="row-action">
              <button
                v-if="!token.revoked_at"
                class="btn ghost sm"
                type="button"
                @click="revoking = token"
              >
                撤销
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="gallery.tokens.length === 0" class="empty muted">还没有分享链接。</p>
    </div>

    <Teleport to="body">
      <div v-if="issued" class="overlay" @click.self="issued = null">
        <div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="分享链接">
          <h2 class="dialog-title">分享链接已生成</h2>
          <p class="muted">请立即复制并发送给客户。关闭后无法再次查看这条链接。</p>
          <label class="field">
            <span>客户链接</span>
            <div class="copy-row">
              <input :value="issued.gallery_url" readonly @focus="($event.target as HTMLInputElement).select()" />
              <button class="btn sm" type="button" @click="copy(issued.gallery_url, '链接')">复制</button>
            </div>
          </label>
          <label class="field">
            <span>令牌（用于排查问题，请勿外传）</span>
            <div class="copy-row">
              <input :value="issued.token" readonly @focus="($event.target as HTMLInputElement).select()" />
              <button class="btn sm" type="button" @click="copy(issued.token, '令牌')">复制</button>
            </div>
          </label>
          <div class="dialog-actions">
            <button class="btn primary" type="button" @click="issued = null">我已保存</button>
          </div>
        </div>
      </div>
    </Teleport>

    <ConfirmDialog
      :open="revoking !== null"
      title="撤销分享链接"
      message="撤销后该链接立即失效，客户已打开的页面会在下一次请求时被拒绝。"
      confirm-text="撤销链接"
      tone="danger"
      :busy="revokeBusy"
      @cancel="revoking = null"
      @confirm="confirmRevoke"
    />
  </section>
</template>
