<script setup lang="ts">
import { computed, inject, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ProofItem } from '../api/proof'
import type { CommentCount } from '../proof/comments'
import { LazyKey } from '../utils/lazy'

const props = defineProps<{
  item: ProofItem
  count: CommentCount
}>()

const emit = defineEmits<{
  (e: 'open'): void
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

const label = computed(() => {
  const parts = [`查看照片 ${props.item.filename}`]
  if (props.item.revision_no > 0) parts.push(`修图版 v${props.item.revision_no}`)
  if (props.count.total > 0) parts.push(`${props.count.total} 条批注`)
  return parts.join('，')
})
</script>

<template>
  <figure ref="root" class="card">
    <button type="button" class="card-open" :aria-label="label" @click="emit('open')">
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
        <span v-if="item.revision_no > 0" class="proof-rev">修图版 v{{ item.revision_no }}</span>
        <span v-if="item.is_video" class="card-video">视频</span>
        <span
          v-if="count.total > 0"
          class="proof-comments"
          :class="{ 'has-open': count.open > 0 }"
          :aria-hidden="true"
        >
          {{ count.total }} 条
        </span>
      </span>
    </button>
  </figure>
</template>
