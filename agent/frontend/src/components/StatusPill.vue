<script setup lang="ts">
import { computed } from 'vue'
import { pauseLabel, statusKind, statusLabel } from '../format'

const props = defineProps<{
  status?: string
  pauseReason?: string
  running?: boolean
}>()

const kind = computed(() => (props.running ? 'info' : statusKind(props.status, props.pauseReason)))
const label = computed(() => {
  if (props.pauseReason) return `已暂停 · ${pauseLabel(props.pauseReason)}`
  if (props.running) return '运行中'
  return statusLabel(props.status)
})
</script>

<template>
  <span class="pill" :class="`pill-${kind}`">
    <span class="dot" />
    {{ label }}
  </span>
</template>
