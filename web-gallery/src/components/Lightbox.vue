<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { PageItem } from '../api/types'
import type { Decision } from '../selection/engine'
import { prefetchImage } from '../utils/lazy'

const props = defineProps<{
  items: PageItem[]
  index: number
  decisions: Record<string, Decision>
  disabled: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'update:index', index: number): void
  (e: 'select'): void
  (e: 'reject'): void
  (e: 'stale'): void
}>()

const closeRef = ref<HTMLButtonElement | null>(null)
const loaded = ref(false)
const failed = ref(false)

const item = computed<PageItem | null>(() => props.items[props.index] ?? null)

const decision = computed<Decision>(() => {
  const current = item.value
  return current ? (props.decisions[current.asset_id] ?? 'none') : 'none'
})

const canPrev = computed(() => props.index > 0)
const canNext = computed(() => props.index < props.items.length - 1)

const placeholderStyle = computed(() => {
  if (item.value?.lqip) return { backgroundImage: `url("${item.value.lqip}")` }
  if (item.value?.color) return { backgroundColor: item.value.color }
  return undefined
})

watch(
  item,
  (next) => {
    loaded.value = false
    failed.value = false
    if (!next) return
    const before = props.items[props.index - 1]
    const after = props.items[props.index + 1]
    if (before) prefetchImage(before.display_url)
    if (after) prefetchImage(after.display_url)
  },
  { immediate: true },
)

watch(
  () => item.value?.display_url,
  () => {
    failed.value = false
    loaded.value = false
  },
)

function go(delta: number) {
  const next = props.index + delta
  if (next < 0 || next >= props.items.length) return
  emit('update:index', next)
}

function onError() {
  loaded.value = false
  if (failed.value) return
  failed.value = true
  emit('stale')
}

let startX = 0
let startY = 0
let tracking = false

function onPointerDown(event: PointerEvent) {
  tracking = true
  startX = event.clientX
  startY = event.clientY
}

function onPointerUp(event: PointerEvent) {
  if (!tracking) return
  tracking = false
  const dx = event.clientX - startX
  const dy = event.clientY - startY
  if (Math.abs(dx) < 48 || Math.abs(dx) < Math.abs(dy)) return
  go(dx > 0 ? -1 : 1)
}

function onKeydown(event: KeyboardEvent) {
  switch (event.key) {
    case 'Escape':
      event.preventDefault()
      emit('close')
      break
    case 'ArrowLeft':
      event.preventDefault()
      go(-1)
      break
    case 'ArrowRight':
      event.preventDefault()
      go(1)
      break
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  closeRef.value?.focus()
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <Teleport to="body">
    <div class="lightbox" role="dialog" aria-modal="true" aria-label="照片查看">
      <div class="lb-head">
        <span class="lb-title">{{ item?.filename ?? '' }}</span>
        <span>{{ index + 1 }} / {{ items.length }}</span>
        <button ref="closeRef" type="button" class="lb-close" aria-label="关闭" @click="emit('close')">
          ✕
        </button>
      </div>

      <div class="lb-stage" @pointerdown="onPointerDown" @pointerup="onPointerUp">
        <span v-if="item && !loaded" class="lb-placeholder" :style="placeholderStyle" />
        <img
          v-if="item"
          :key="item.asset_id"
          :src="item.display_url"
          class="lb-image"
          :class="{ 'is-hidden': !loaded }"
          :alt="item.filename"
          decoding="async"
          draggable="false"
          @load="loaded = true"
          @error="onError"
        />
        <span v-if="item && !loaded && !failed" class="lb-spinner">加载中…</span>
        <span v-if="failed" class="lb-spinner">图片加载失败，正在重新获取…</span>

        <button v-if="canPrev" type="button" class="lb-nav prev" aria-label="上一张" @click="go(-1)">
          ‹
        </button>
        <button v-if="canNext" type="button" class="lb-nav next" aria-label="下一张" @click="go(1)">
          ›
        </button>
        <span v-if="item?.is_video" class="lb-note">视频将于交付阶段提供，这里显示封面帧</span>
      </div>

      <div class="lb-foot">
        <button
          type="button"
          class="lb-action"
          :class="{ 'is-on': decision === 'selected' }"
          :aria-pressed="decision === 'selected'"
          :disabled="disabled"
          @click="emit('select')"
        >
          ✓ 收藏
        </button>
        <button
          type="button"
          class="lb-action"
          :class="{ 'is-off': decision === 'rejected' }"
          :aria-pressed="decision === 'rejected'"
          :disabled="disabled"
          @click="emit('reject')"
        >
          ✕ 不选
        </button>
        <span v-if="disabled" class="lb-hint">已提交，无法修改</span>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.lb-image {
  transition: opacity 0.2s ease;
}

.lb-image.is-hidden {
  opacity: 0;
}
</style>
