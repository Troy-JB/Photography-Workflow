<script setup lang="ts">
import { dismiss, toasts } from '../state/toast'
</script>

<template>
  <div class="toast-host" aria-live="polite">
    <TransitionGroup name="toast">
      <div
        v-for="entry in toasts"
        :key="entry.id"
        class="toast"
        :class="entry.kind"
        role="status"
        @click="dismiss(entry.id)"
      >
        {{ entry.message }}
      </div>
    </TransitionGroup>
  </div>
</template>
