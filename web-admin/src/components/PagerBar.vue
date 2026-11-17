<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ total: number; limit: number; offset: number; busy?: boolean }>()
const emit = defineEmits<{ change: [offset: number] }>()

const page = computed(() => Math.floor(props.offset / props.limit) + 1)
const pages = computed(() => Math.max(1, Math.ceil(props.total / props.limit)))
const canPrev = computed(() => props.offset > 0)
const canNext = computed(() => props.offset + props.limit < props.total)

function go(delta: number): void {
  if (props.busy) return
  emit('change', Math.max(0, props.offset + delta * props.limit))
}
</script>

<template>
  <div v-if="total > limit" class="pager">
    <span class="muted small">共 {{ total }} 项 · 第 {{ page }} / {{ pages }} 页</span>
    <div class="pager-buttons">
      <button class="btn ghost sm" type="button" :disabled="!canPrev || busy" @click="go(-1)">
        上一页
      </button>
      <button class="btn ghost sm" type="button" :disabled="!canNext || busy" @click="go(1)">
        下一页
      </button>
    </div>
  </div>
</template>
