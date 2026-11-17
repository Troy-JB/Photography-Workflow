<script setup lang="ts">
import { computed } from 'vue'
import type { ProofTerminalCode } from '../state/proof'

const props = defineProps<{ code: ProofTerminalCode }>()

const copy = computed(() => {
  switch (props.code) {
    case 'expired':
      return {
        mark: 'EXPIRED',
        title: '审片链接已过期',
        body: '该审片链接已过有效期。如需继续审阅，请联系工作室重新发送。',
      }
    case 'revoked':
      return {
        mark: 'REVOKED',
        title: '审片链接已停用',
        body: '工作室已停用该审片链接。如果您仍需要查看照片，请联系工作室。',
      }
    case 'too_many_visits':
      return {
        mark: 'LIMIT',
        title: '访问次数已达上限',
        body: '为了保障照片安全，该链接的可访问次数已用完。请联系工作室重新获取。',
      }
    case 'gallery_revoked':
      return {
        mark: 'WITHDRAWN',
        title: '相册已撤回',
        body: '工作室已撤回对应相册，审片通道随之关闭。如有疑问，请联系工作室。',
      }
    case 'round_closed':
      return {
        mark: 'CLOSED',
        title: '审片已关闭',
        body: '本轮审片已结束或不可访问。如需继续处理，请联系工作室。',
      }
    case 'invalid_token':
    default:
      return {
        mark: 'INVALID',
        title: '审片链接无效',
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
