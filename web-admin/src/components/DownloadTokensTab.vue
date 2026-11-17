<script setup lang="ts">
import { computed, ref } from 'vue'
import { ApiError } from '../api/client'
import type { DeliveryToken, IssuedDeliveryToken } from '../api/delivery'
import {
  issueDeliveryToken,
  revokeAllDeliveryTokens,
  revokeDeliveryToken,
} from '../api/delivery'
import ConfirmDialog from './ConfirmDialog.vue'
import { toast } from '../state/toast'
import { copyText } from '../utils/clipboard'
import { formatDateTime } from '../utils/format'
import { parseNumberInput } from '../utils/number'

const props = defineProps<{
  deliveryId: string
  tokens: DeliveryToken[]
  canManage: boolean
  ready: boolean
  paymentOk: boolean
}>()

const emit = defineEmits<{ refresh: [] }>()

const label = ref('')
const expiresInDays = ref('')
const maxDownloads = ref('')
const issuing = ref(false)
const issueError = ref('')
const oneTime = ref<IssuedDeliveryToken | null>(null)

const revoking = ref<DeliveryToken | null>(null)
const revokeBusy = ref(false)
const revokeAllAsk = ref(false)
const revokeAllBusy = ref(false)

const canIssue = computed(() => props.ready && props.paymentOk && props.canManage)
const activeCount = computed(() => props.tokens.filter((token) => !token.revoked_at).length)

function tokenState(token: DeliveryToken): { label: string; tone: string } {
  if (token.revoked_at) return { label: '已撤销', tone: 'warn' }
  if (Date.parse(token.expires_at) <= Date.now()) return { label: '已过期', tone: 'muted' }
  if (token.downloads >= token.max_downloads) return { label: '次数用尽', tone: 'danger' }
  return { label: '有效', tone: 'ok' }
}

function optionalInt(raw: string, name: string, min: number, max: number): number | string {
  const parsed = parseNumberInput(raw)
  if (parsed.empty) return 0
  if (!Number.isInteger(parsed.value) || parsed.value < min || parsed.value > max) {
    return `${name}必须是 ${min}–${max} 之间的整数`
  }
  return parsed.value
}

async function issue(): Promise<void> {
  if (issuing.value) return
  const days = optionalInt(expiresInDays.value, '有效期', 1, 30)
  if (typeof days === 'string') {
    issueError.value = days
    return
  }
  const downloads = optionalInt(maxDownloads.value, '下载次数', 1, 100)
  if (typeof downloads === 'string') {
    issueError.value = downloads
    return
  }
  issuing.value = true
  issueError.value = ''
  try {
    const issued = await issueDeliveryToken(props.deliveryId, {
      label: label.value.trim(),
      expires_in_days: days,
      max_downloads: downloads,
    })
    oneTime.value = issued
    label.value = ''
    expiresInDays.value = ''
    maxDownloads.value = ''
    emit('refresh')
  } catch (err) {
    issueError.value = err instanceof ApiError ? err.detail || err.message : '签发链接失败'
  } finally {
    issuing.value = false
  }
}

async function confirmRevoke(): Promise<void> {
  const token = revoking.value
  if (!token) return
  revokeBusy.value = true
  try {
    await revokeDeliveryToken(props.deliveryId, token.id)
    toast('下载链接已撤销，客户下一次请求即失效', 'success')
    revoking.value = null
    emit('refresh')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '撤销失败', 'error')
  } finally {
    revokeBusy.value = false
  }
}

async function confirmRevokeAll(): Promise<void> {
  revokeAllBusy.value = true
  try {
    const result = await revokeAllDeliveryTokens(props.deliveryId)
    toast(`已撤销 ${result.revoked} 个有效链接`, 'success')
    revokeAllAsk.value = false
    emit('refresh')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '批量撤销失败', 'error')
  } finally {
    revokeAllBusy.value = false
  }
}

async function copy(value: string, what: string): Promise<void> {
  const ok = await copyText(value)
  toast(ok ? `${what}已复制` : '复制失败，请手动选择文本复制', ok ? 'success' : 'error')
}
</script>

<template>
  <div class="panel">
    <div class="panel-head">
      <h2 class="panel-title">下载链接</h2>
      <button
        v-if="canManage && activeCount > 0"
        class="btn danger sm"
        type="button"
        @click="revokeAllAsk = true"
      >
        全部撤销
      </button>
    </div>

    <p v-if="!paymentOk" class="alert warn">
      项目尾款尚未结清，构建、签发与下载均已阻断。请先在项目中收款。
    </p>
    <p v-else-if="!ready" class="alert warn">
      交付还未就绪，先完成构建后才能签发下载链接。
    </p>

    <form class="token-form" @submit.prevent="issue">
      <label class="field">
        <span>备注</span>
        <input v-model="label" type="text" maxlength="80" placeholder="例如：客户微信" :disabled="!canIssue" />
      </label>
      <label class="field">
        <span>有效期（天，默认 14）</span>
        <input v-model="expiresInDays" type="number" min="1" max="30" step="1" placeholder="14" :disabled="!canIssue" />
      </label>
      <label class="field">
        <span>下载次数上限（默认 20）</span>
        <input v-model="maxDownloads" type="number" min="1" max="100" step="1" placeholder="20" :disabled="!canIssue" />
      </label>
      <button class="btn primary" type="submit" :disabled="issuing || !canIssue">
        {{ issuing ? '签发中…' : '签发链接' }}
      </button>
    </form>
    <p v-if="issueError" class="form-error" role="alert">{{ issueError }}</p>
    <p class="muted small token-hint">
      明文链接仅在签发后显示一次，数据库只保存 SHA-256；每次下载都会消耗一次额度，撤销后下一次请求立即失效。
    </p>

    <table class="data compact">
      <thead>
        <tr>
          <th>备注</th>
          <th>状态</th>
          <th class="num">下载次数</th>
          <th>过期时间</th>
          <th>最近使用</th>
          <th>创建时间</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="token in tokens" :key="token.id">
          <td>
            {{ token.label || '未命名' }}
            <div v-if="token.recipient" class="muted small">{{ token.recipient }}</div>
          </td>
          <td><span class="pill" :class="tokenState(token).tone">{{ tokenState(token).label }}</span></td>
          <td class="num">
            {{ token.downloads }}<span class="muted"> / {{ token.max_downloads }}</span>
          </td>
          <td>{{ formatDateTime(token.expires_at) }}</td>
          <td>{{ formatDateTime(token.last_used_at) }}</td>
          <td>{{ formatDateTime(token.created_at) }}</td>
          <td class="row-action">
            <button
              v-if="!token.revoked_at && canManage"
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
    <p v-if="tokens.length === 0" class="muted small">还没有签发下载链接。</p>

    <Teleport to="body">
      <div v-if="oneTime" class="overlay" @click.self="oneTime = null">
        <div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="下载链接已签发">
          <h2 class="dialog-title">下载链接已签发</h2>
          <p class="muted">明文链接只显示一次，数据库仅保存哈希；撤销令牌后旧链接立即失效。</p>
          <label class="field">
            <span>客户下载链接</span>
            <div class="copy-row">
              <input :value="oneTime.url" readonly @focus="($event.target as HTMLInputElement).select()" />
              <button class="btn sm" type="button" @click="copy(oneTime!.url, '链接')">复制</button>
            </div>
          </label>
          <p class="muted small">
            有效期至 {{ formatDateTime(oneTime.expires_at) }} · 可下载 {{ oneTime.max_downloads }} 次
          </p>
          <div class="dialog-actions">
            <button class="btn primary" type="button" @click="oneTime = null">我已保存</button>
          </div>
        </div>
      </div>
    </Teleport>

    <ConfirmDialog
      :open="revoking !== null"
      title="撤销下载链接"
      message="撤销后该链接立即失效，客户已打开的页面会在下一次请求时被拒绝。"
      confirm-text="撤销链接"
      tone="danger"
      :busy="revokeBusy"
      @cancel="revoking = null"
      @confirm="confirmRevoke"
    />

    <ConfirmDialog
      :open="revokeAllAsk"
      title="撤销全部下载链接"
      :message="`将撤销 ${activeCount} 个有效链接。用于链接泄露等事故处置，客户需要新链接才能继续下载。`"
      confirm-text="全部撤销"
      tone="danger"
      :busy="revokeAllBusy"
      @cancel="revokeAllAsk = false"
      @confirm="confirmRevokeAll"
    />
  </div>
</template>
