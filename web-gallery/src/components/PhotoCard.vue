<script setup lang="ts">
import { computed, inject, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { PageItem } from '../api/types'
import type { Decision } from '../selection/engine'
import { LazyKey } from '../utils/lazy'

const props = defineProps<{
  item: PageItem
  decision: Decision
  disabled: boolean
}>()

const emit = defineEmits<{
  (e: 'open'): void
  (e: 'select'): void
  (e: 'reject'): void
  (e: 'stale'): void
}>()

const root = ref<HTMLElement | null>(null)
const armed = ref(false)
const loaded = ref(false)
const failed = ref(false)
const lazy = inject(LazyKey, null)

let staleRequests = 0

onMounted(() => {
  if (!lazy || !root.value) {
    armed.value = true
    return
  }
  lazy.observe(root.value, () => {
    armed.value = true
  })
})

onBeforeUnmount(() => {
  if (lazy && root.value) lazy.release(root.value)
})

watch(
  () => props.item.thumb_url,
  () => {
    if (!failed.value) return
    failed.value = false
    loaded.value = false
  },
)

function onError() {
  loaded.value = false
  if (staleRequests >= 2) return
  staleRequests++
  failed.value = true
  emit('stale')
}

const placeholderStyle = computed(() => {
  if (props.item.lqip) return { backgroundImage: `url("${props.item.lqip}")` }
  if (props.item.color) return { backgroundColor: props.item.color }
  return undefined
})

const stateLabel = computed(() => {
  if (props.decision === 'selected') return '已选'
  if (props.decision === 'rejected') return '已弃'
  return ''
})
</script>

<template>
  <figure
    ref="root"
    class="card"
    :class="{
      'is-selected': decision === 'selected',
      'is-rejected': decision === 'rejected',
    }"
  >
    <button
      type="button"
      class="card-open"
      :aria-label="`查看照片 ${item.filename}`"
      @click="emit('open')"
    >
      <span class="card-media">
        <img
          v-if="armed"
          :src="item.thumb_url"
          :class="{ 'is-loaded': loaded }"
          :alt="item.filename"
          decoding="async"
          @load="loaded = true"
          @error="onError"
        />
        <span v-if="!loaded" class="card-placeholder" :style="placeholderStyle" />
        <span v-if="item.is_video" class="card-video">视频</span>
        <span v-if="stateLabel" class="card-state">{{ stateLabel }}</span>
        <span class="card-brand-watermark" aria-hidden="true">Lumeo</span>
      </span>
    </button>

    <div class="card-actions">
      <button
        type="button"
        class="card-action select"
        :class="{ active: decision === 'selected' }"
        :aria-label="decision === 'selected' ? '取消收藏' : '收藏这张照片'"
        :aria-pressed="decision === 'selected'"
        :disabled="disabled"
        @click.stop="emit('select')"
      >
        ✓
      </button>
      <button
        type="button"
        class="card-action reject"
        :class="{ active: decision === 'rejected' }"
        :aria-label="decision === 'rejected' ? '取消舍弃' : '舍弃这张照片'"
        :aria-pressed="decision === 'rejected'"
        :disabled="disabled"
        @click.stop="emit('reject')"
      >
        ✕
      </button>
    </div>
  </figure>
</template>
