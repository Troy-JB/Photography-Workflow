<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ApiError } from '../api/client'
import { getRetentionPolicy, updateRetentionPolicy } from '../api/archive'
import { auth } from '../state/session'
import { toast } from '../state/toast'
import { formatDateTime } from '../utils/format'
import { parseNumberInput } from '../utils/number'

const keepDays = ref('')
const warnDays = ref('')
const updatedAt = ref<string | null>(null)
const loading = ref(true)
const saving = ref(false)
const loadError = ref('')
const formError = ref('')

const isOwner = computed(() => auth.role === 'owner')

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    const policy = await getRetentionPolicy()
    keepDays.value = String(policy.keep_days)
    warnDays.value = String(policy.warn_days)
    updatedAt.value = policy.updated_at ?? null
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.detail || err.message : '加载保留策略失败'
  } finally {
    loading.value = false
  }
}

function parseDays(raw: string, min: number, max: number): number | string {
  const parsed = parseNumberInput(raw)
  if (parsed.empty) return '不能为空'
  if (!Number.isInteger(parsed.value) || parsed.value < min || parsed.value > max) {
    return `必须是 ${min}–${max} 之间的整数`
  }
  return parsed.value
}

async function save(): Promise<void> {
  if (saving.value) return
  formError.value = ''
  const keep = parseDays(keepDays.value, 1, 3650)
  if (typeof keep === 'string') {
    formError.value = `保留天数${keep}`
    return
  }
  const warn = parseDays(warnDays.value, 0, 3649)
  if (typeof warn === 'string') {
    formError.value = `提醒天数${warn}`
    return
  }
  if (warn >= keep) {
    formError.value = '提前提醒天数必须小于保留天数'
    return
  }
  saving.value = true
  try {
    const policy = await updateRetentionPolicy(keep, warn)
    keepDays.value = String(policy.keep_days)
    warnDays.value = String(policy.warn_days)
    updatedAt.value = policy.updated_at ?? null
    toast('保留策略已保存，新创建的归档会使用该策略', 'success')
  } catch (err) {
    formError.value = err instanceof ApiError ? err.detail || err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1>保留策略</h1>
        <p class="muted">
          租户级归档保留设置。策略在创建归档时快照，修改不会影响已有归档的到期日；到期前只发送一次提醒，不会自动删除。
        </p>
      </div>
    </header>

    <p v-if="loadError" class="alert error" role="alert">{{ loadError }}</p>
    <p v-else-if="loading" class="empty muted">加载中…</p>

    <div v-if="!loading && !loadError" class="panel">
      <h2 class="panel-title">策略参数</h2>
      <p v-if="!isOwner" class="alert warn">只有 owner 可以修改保留策略。</p>
      <form class="inline-form" @submit.prevent="save">
        <label class="field">
          <span>保留天数（1–3650）</span>
          <input v-model="keepDays" type="number" min="1" max="3650" step="1" :disabled="!isOwner" />
        </label>
        <label class="field">
          <span>提前提醒天数（必须小于保留天数）</span>
          <input v-model="warnDays" type="number" min="0" max="3649" step="1" :disabled="!isOwner" />
        </label>
        <button class="btn primary" type="submit" :disabled="saving || !isOwner">
          {{ saving ? '保存中…' : '保存策略' }}
        </button>
      </form>
      <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>
      <p class="muted small">
        默认保留 365 天、提前 30 天提醒。
        <template v-if="updatedAt">最近修改：{{ formatDateTime(updatedAt) }}。</template>
        删除归档对象是单独的事故处置动作，需要 owner 输入 DELETE 并填写原因，全程写入审计日志。
      </p>
    </div>
  </section>
</template>
