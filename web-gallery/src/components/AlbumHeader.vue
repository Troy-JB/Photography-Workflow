<script setup lang="ts">
import { computed } from 'vue'
import type { Counts } from '../api/types'
import type { Countdown } from '../utils/countdown'
import type { Filter } from '../state/filter'

const props = defineProps<{
  title: string
  submittedAt: string | null
  countdown: Countdown | null
  limit: number | null
  counts: Counts
  saving: boolean
  pending: number
  canSubmit: boolean
  submitting: boolean
  filter: Filter
  editCountdown: Countdown | null
  submitted: boolean
  extraSelected: number
  extraAmountCents: number
  paymentUrl: string
}>()

const emit = defineEmits<{
  (e: 'submit'): void
  (e: 'filter-change', filter: Filter): void
}>()

const urgent = computed(
  () => Boolean(props.countdown && !props.countdown.expired && props.countdown.days === 0),
)

const deadlineText = computed(() => {
  const value = props.countdown
  if (!value) return ''
  return value.expired ? '选片已截止' : `距截止 ${value.text}`
})

const options: { key: Filter; label: string; count: number }[] = [
  { key: 'all', label: '全部', count: 0 },
  { key: 'selected', label: '已选', count: 0 },
  { key: 'rejected', label: '已弃', count: 0 },
]

const filters = computed(() =>
  options.map((option) => ({
    ...option,
    count:
      option.key === 'all'
        ? props.counts.assets
        : option.key === 'selected'
          ? props.counts.selected
          : props.counts.rejected,
  })),
)

const busy = computed(() => props.saving || props.pending > 0)
</script>

<template>
  <header class="topbar">
    <div class="page-inner topbar-inner">
      <div class="topbar-row">
        <div>
          <p class="brand-mark">PWF <strong>SELECTION</strong></p>
          <h1 class="album-title">{{ title }}</h1>
          <p class="album-sub">
            <span
              v-if="countdown"
              class="countdown"
              :class="{ 'is-urgent': urgent, 'is-expired': countdown.expired }"
            >
              {{ deadlineText }}
            </span>
            <span v-if="limit !== null">选片上限 {{ limit }} 张</span>
            <span v-if="submitted && editCountdown && !editCountdown.expired">可调整 {{ editCountdown.text }}</span>
            <span v-if="extraSelected > 0" class="overage-note">超出套餐 {{ extraSelected }} 张 · 补款 ¥{{ (extraAmountCents / 100).toFixed(2) }}</span>
          </p>
        </div>
        <div class="header-actions">
          <span v-if="busy" class="save-hint"><span class="save-dot" />保存中…</span>
          <button
            v-if="!submittedAt || canSubmit"
            type="button"
            class="btn btn-primary"
            :disabled="!canSubmit || busy"
            @click="emit('submit')"
          >
            {{ submitting ? '提交中…' : submitted ? '确认本次选择' : '提交选片' }}
          </button>
        </div>
      </div>

      <div class="stats">
        <div class="stat">
          <strong>{{ counts.assets }}</strong>
          <span>共</span>
        </div>
        <div class="stat is-accent">
          <strong>{{ counts.selected }}</strong>
          <span>已选</span>
        </div>
        <div class="stat">
          <strong>{{ counts.rejected }}</strong>
          <span>已弃</span>
        </div>
        <div class="stat">
          <strong>{{ counts.undecided }}</strong>
          <span>未定</span>
        </div>
      </div>

      <div v-if="paymentUrl" class="supplement-payment" role="status">
        <span>超额精修费用已生成，请先完成支付</span>
        <a class="btn btn-primary" :href="paymentUrl" target="_blank" rel="noreferrer">支付补选费用</a>
      </div>

      <div class="filters" role="tablist" aria-label="筛选照片">
        <button
          v-for="option in filters"
          :key="option.key"
          type="button"
          role="tab"
          class="filter"
          :class="{ 'is-active': filter === option.key }"
          :aria-selected="filter === option.key"
          @click="emit('filter-change', option.key)"
        >
          {{ option.label }}
          <span class="filter-count">{{ option.count }}</span>
        </button>
      </div>
    </div>
  </header>
</template>
