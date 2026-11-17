<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { Lead, LeadStatus, ListResponse } from '../api/types'
import { api } from '../state/session'
import { formatDateTime } from '../utils/format'

const rows = ref<Lead[]>([])
const total = ref(0)
const selectedId = ref('')
const status = ref('')
const query = ref('')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const saved = ref(false)
const emailDraft = ref('')

const selected = computed(() => rows.value.find((item) => item.id === selectedId.value) ?? null)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const params = new URLSearchParams({ limit: '100', offset: '0' })
    if (status.value) params.set('status', status.value)
    if (query.value.trim()) params.set('q', query.value.trim())
    const page = await api<ListResponse<Lead>>(`/api/v1/leads?${params}`)
    rows.value = page.items
    total.value = page.total
    if (!rows.value.some((item) => item.id === selectedId.value)) selectedId.value = rows.value[0]?.id ?? ''
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '线索加载失败'
  } finally {
    loading.value = false
  }
}

watch(selected, (lead) => {
  emailDraft.value = lead?.email ?? ''
  saved.value = false
}, { flush: 'sync' })
watch([status, query], () => void load())

async function saveEmail(): Promise<void> {
  if (!selected.value) return
  saving.value = true
  error.value = ''
  saved.value = false
  try {
    const updated = await api<Lead>(`/api/v1/leads/${selected.value.id}`, {
      method: 'PATCH',
      body: { expected_version: selected.value.version, email: emailDraft.value },
    })
    rows.value = rows.value.map((item) => item.id === updated.id ? updated : item)
    saved.value = true
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '邮箱保存失败'
  } finally {
    saving.value = false
  }
}

const statuses: { value: LeadStatus; label: string }[] = [
  { value: 'new', label: '新建' },
  { value: 'contacted', label: '已联系' },
  { value: 'qualified', label: '已确认需求' },
  { value: 'quoted', label: '已报价' },
  { value: 'won', label: '已成交' },
  { value: 'lost', label: '已关闭' },
  { value: 'converted', label: '已转客户' },
]

function statusLabel(value: LeadStatus): string {
  return statuses.find((item) => item.value === value)?.label ?? value
}

onMounted(() => void load())
</script>

<template>
  <section class="page leads-page">
    <header class="page-head">
      <div>
        <h1>线索</h1>
        <p class="muted">{{ total }} 条线索 · 管理官网与工作室咨询</p>
      </div>
    </header>

    <div v-if="error" class="notice error">{{ error }}</div>

    <div class="leads-layout">
      <section class="panel leads-list" aria-label="线索列表">
        <div class="toolbar">
          <label class="inline-field">
            <span>状态</span>
            <select v-model="status">
              <option value="">全部状态</option>
              <option v-for="item in statuses" :key="item.value" :value="item.value">{{ item.label }}</option>
            </select>
          </label>
          <label class="inline-field leads-search">
            <span>搜索</span>
            <input v-model="query" type="search" placeholder="姓名、邮箱或需求" />
          </label>
        </div>
        <div v-if="loading" class="empty-state">正在加载线索…</div>
        <div v-else-if="!rows.length" class="empty-state">暂无线索</div>
        <div v-else class="lead-rows">
          <button
            v-for="lead in rows"
            :key="lead.id"
            class="lead-row"
            :class="{ selected: selectedId === lead.id }"
            type="button"
            @click="selectedId = lead.id"
          >
            <span class="lead-row-top"><strong>{{ lead.name }}</strong><span class="status-text">{{ statusLabel(lead.status) }}</span></span>
            <span class="lead-row-email">{{ lead.email || '未填写邮箱' }}</span>
            <span class="lead-row-date">{{ formatDateTime(lead.created_at) }}</span>
          </button>
        </div>
      </section>

      <section v-if="selected" class="panel lead-detail" aria-label="线索详情">
        <div class="lead-detail-head">
          <div>
            <div class="eyebrow">{{ selected.source === 'website' ? '官网提交' : '工作室线索' }}</div>
            <h2>{{ selected.name }}</h2>
          </div>
          <span class="status-text">{{ statusLabel(selected.status) }}</span>
        </div>
        <dl class="lead-meta">
          <div><dt>收到时间</dt><dd>{{ formatDateTime(selected.created_at) }}</dd></div>
          <div><dt>需求</dt><dd>{{ selected.need || '—' }}</dd></div>
          <div><dt>公司</dt><dd>{{ selected.company || '—' }}</dd></div>
          <div><dt>电话</dt><dd>{{ selected.phone || '—' }}</dd></div>
        </dl>
        <label class="field-label" for="lead-email">邮箱</label>
        <input id="lead-email" v-model="emailDraft" type="email" maxlength="254" autocomplete="email" />
        <p v-if="saved" class="success-copy" role="status">邮箱已保存</p>
        <div class="lead-actions">
          <a v-if="selected.email" class="btn ghost" :href="`mailto:${selected.email}`">发送邮件</a>
          <button class="btn primary" type="button" :disabled="saving" @click="saveEmail">
            {{ saving ? '保存中…' : '保存邮箱' }}
          </button>
        </div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.leads-layout { display: grid; grid-template-columns: minmax(0, 1.15fr) minmax(280px, 0.85fr); gap: 1rem; align-items: start; }
.leads-list, .lead-detail { min-width: 0; }
.leads-search { flex: 1; }
.lead-rows { display: grid; }
.lead-row { display: grid; gap: 0.2rem; width: 100%; padding: 0.85rem 1rem; border: 0; border-top: 1px solid var(--border); background: transparent; text-align: left; transition: background 160ms ease; }
.lead-row:hover, .lead-row.selected { background: var(--panel-strong); }
.lead-row-top { display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; text-transform: capitalize; }
.lead-row-email, .lead-row-date, .status-text { color: var(--muted); font-size: 12px; }
.lead-detail-head { display: flex; align-items: start; justify-content: space-between; gap: 1rem; margin-bottom: 1.5rem; }
.lead-detail h2 { margin-top: 0.35rem; font-size: 20px; }
.eyebrow { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: 0.1em; }
.lead-meta { display: grid; grid-template-columns: 1fr 1fr; gap: 1rem; margin-bottom: 1.5rem; }
.lead-meta div { min-width: 0; }
.lead-meta dt { color: var(--muted); font-size: 11px; }
.lead-meta dd { margin: 0.2rem 0 0; overflow-wrap: anywhere; }
.field-label { display: block; margin-bottom: 0.35rem; font-size: 12px; }
.lead-actions { display: flex; justify-content: flex-end; gap: 0.5rem; margin-top: 1rem; }
.success-copy { margin-top: 0.45rem; color: var(--ok); font-size: 12px; }
.empty-state { padding: 2rem 1rem; color: var(--muted); text-align: center; }
@media (max-width: 760px) { .leads-layout { grid-template-columns: 1fr; } }
</style>
