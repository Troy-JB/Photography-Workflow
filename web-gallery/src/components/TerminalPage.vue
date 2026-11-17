<script setup lang="ts">
import { computed } from 'vue'
import type { TerminalCode } from '../state/session'

const props = defineProps<{ code: TerminalCode; detail?: string }>()

const copy = computed(() => {
  switch (props.code) {
    case 'expired':
      return {
        mark: 'EXPIRED',
        title: '链接已过期',
        body: '分享链接已过有效期。如需继续选片，请联系工作室重新发送。',
      }
    case 'revoked':
      return {
        mark: 'REVOKED',
        title: '链接已停用',
        body: '工作室已停用该分享链接。如果您仍需要查看相册，请联系工作室。',
      }
    case 'too_many_visits':
      return {
        mark: 'LIMIT',
        title: '访问次数已达上限',
        body: '为了保障照片安全，该链接的可访问次数已用完。请联系工作室重新获取。',
      }
    case 'gallery_expired':
      return {
        mark: 'CLOSED',
        title: '选片已截止',
        body: '本相册的选片截止时间已到。如需延期，请联系工作室。',
      }
    case 'gallery_revoked':
      return {
        mark: 'WITHDRAWN',
        title: '相册已撤回',
        body: '工作室已撤回本相册。如有疑问，请联系工作室。',
      }
    case 'balance_unpaid':
      return {
        mark: 'BALANCE DUE',
        title: '请先支付尾款',
        body: '原片已经准备好。请在客户中心完成尾款支付，支付成功后重新打开本链接即可选片。',
      }
    case 'invalid_token':
    default:
      return {
        mark: 'INVALID',
        title: '链接无效',
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
        <p v-if="detail" class="state-detail">{{ detail }}</p>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>
</template>

<style scoped>
.state-detail {
  margin-top: 10px;
  color: #705c50;
  font-size: 12px;
}

.state-mark {
  letter-spacing: 0.18em;
  font-size: 10px;
  font-weight: 600;
}
</style>
