<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    message: string
    confirmText?: string
    cancelText?: string
    tone?: 'default' | 'danger'
    busy?: boolean
  }>(),
  { confirmText: '确认', cancelText: '取消', tone: 'default', busy: false },
)

const emit = defineEmits<{ confirm: []; cancel: [] }>()

const confirmButton = ref<HTMLButtonElement | null>(null)

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    await nextTick()
    confirmButton.value?.focus()
  },
)

function onKeydown(event: KeyboardEvent): void {
  if (!props.open || props.busy) return
  if (event.key === 'Escape') {
    event.stopPropagation()
    emit('cancel')
  }
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay" @keydown="onKeydown" @click.self="!busy && emit('cancel')">
      <div class="dialog" role="dialog" aria-modal="true" :aria-label="title" tabindex="-1">
        <h2 class="dialog-title">{{ title }}</h2>
        <p class="dialog-message">{{ message }}</p>
        <div class="dialog-actions">
          <button class="btn ghost" type="button" :disabled="busy" @click="emit('cancel')">
            {{ cancelText }}
          </button>
          <button
            ref="confirmButton"
            class="btn"
            :class="tone === 'danger' ? 'danger' : 'primary'"
            type="button"
            :disabled="busy"
            @click="emit('confirm')"
          >
            {{ busy ? '处理中…' : confirmText }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
