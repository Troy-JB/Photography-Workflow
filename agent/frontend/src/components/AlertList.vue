<script setup lang="ts">
import type { AlertEvent } from '../types'
import { formatTime } from '../format'

defineProps<{ alerts: AlertEvent[]; canAck?: boolean }>()
defineEmits<{ ack: [id: number] }>()
</script>

<template>
  <ul class="alert-list">
    <li v-for="alert in alerts" :key="alert.id" class="alert" :class="`alert-${alert.level}`">
      <div class="alert-body">
        <span class="alert-msg">{{ alert.message }}</span>
        <span class="alert-meta">{{ formatTime(alert.created_at) }} · {{ alert.code }}</span>
      </div>
      <button v-if="canAck" class="btn tiny ghost" @click="$emit('ack', alert.id)">确认</button>
    </li>
  </ul>
</template>
