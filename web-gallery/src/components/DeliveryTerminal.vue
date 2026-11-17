<script setup lang="ts">
import { computed } from 'vue'
import type { DeliveryTerminalCode } from '../state/delivery'

const props = defineProps<{ code: DeliveryTerminalCode }>()

const copy = computed(() => {
  switch (props.code) {
    case 'link_expired':
      return {
        mark: 'EXPIRED',
        title: '下载链接已过期',
        body: '该下载链接已过有效期。如需继续下载，请联系工作室重新发送。',
      }
    case 'link_revoked':
      return {
        mark: 'REVOKED',
        title: '下载链接已停用',
        body: '工作室已停用该下载链接。如果您仍需要交付文件，请联系工作室。',
      }
    case 'payment_required':
      return {
        mark: 'UNSETTLED',
        title: '尾款尚未结清',
        body: '项目的尾款尚未结清，交付下载暂未开放。请联系工作室完成结算后再打开本链接。',
      }
    case 'delivery_not_ready':
      return {
        mark: 'PACKING',
        title: '交付文件准备中',
        body: '工作室正在打包交付文件，完成后即可通过本链接下载。请稍后重试或联系工作室。',
      }
    case 'round_not_approved':
      return {
        mark: 'PENDING',
        title: '审片尚未通过',
        body: '该项目的审片尚未确认通过，交付下载暂未开放。请联系工作室了解进度。',
      }
    case 'downloads_exhausted':
      return {
        mark: 'LIMIT',
        title: '下载次数已用完',
        body: '该链接的下载次数已用完。如需继续下载，请联系工作室重新发送下载链接。',
      }
    case 'delivery_archived':
      return {
        mark: 'ARCHIVED',
        title: '项目已归档',
        body: '该项目的交付文件已归档，下载链接不再可用。如有需要，请联系工作室恢复。',
      }
    case 'delivery_cancelled':
      return {
        mark: 'CANCELLED',
        title: '项目已取消',
        body: '该项目已取消，交付下载不再可用。如有疑问，请联系工作室。',
      }
    case 'invalid_link':
    default:
      return {
        mark: 'INVALID',
        title: '下载链接无效',
        body: '这个链接无法打开，可能不完整或已被更换。请向工作室确认后重新打开。',
      }
  }
})
</script>

<template>
  <div class="page">
    <div class="centered-page">
      <div class="state-card">
        <div class="state-mark">{{ copy.mark }}</div>
        <h1>{{ copy.title }}</h1>
        <p>{{ copy.body }}</p>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>
</template>

<style scoped>
.state-mark {
  letter-spacing: 0.18em;
  font-size: 10px;
  font-weight: 600;
}
</style>
