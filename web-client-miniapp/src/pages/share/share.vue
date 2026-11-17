<script setup lang="ts">
import { onLoad, onUnload } from '@dcloudio/uni-app'
import { ref } from 'vue'
import { request } from '../../api'
import { isCustomerShareURL, selectionShareToken } from '../../share'

interface WeChatScreenBridge {
  onScreenRecordingStateChanged?: (listener: (result: { state: 'start' | 'stop' }) => void) => void
  offScreenRecordingStateChanged?: (listener: (result: { state: 'start' | 'stop' }) => void) => void
}

const url = ref('')
const error = ref('')
const captureGuard = ref(false)
let captureTimer: ReturnType<typeof setTimeout> | undefined

function screenBridge(): WeChatScreenBridge | undefined {
  return (globalThis as unknown as { wx?: WeChatScreenBridge }).wx
}

function onCapture(): void {
  captureGuard.value = true
  const token = selectionShareToken(url.value)
  if (token) void request('/g/v1/capture-events', 'POST', { token, event: 'screen_capture' }).catch(() => undefined)
  if (captureTimer) clearTimeout(captureTimer)
  captureTimer = setTimeout(() => { captureGuard.value = false }, 2200)
}

function onRecordingChange(result: { state: 'start' | 'stop' }): void {
  captureGuard.value = result.state === 'start'
  if (result.state !== 'start') {
    if (captureTimer) clearTimeout(captureTimer)
    return
  }
  const token = selectionShareToken(url.value)
  if (token) void request('/g/v1/capture-events', 'POST', { token, event: 'screen_recording_start' }).catch(() => undefined)
  if (captureTimer) clearTimeout(captureTimer)
}

onLoad((query) => {
  if (typeof uni.onUserCaptureScreen === 'function') uni.onUserCaptureScreen(onCapture)
  screenBridge()?.onScreenRecordingStateChanged?.(onRecordingChange)
  const value = decodeURIComponent(String(query?.url ?? ''))
  if (!isCustomerShareURL(value)) {
    error.value = '链接格式不支持，请粘贴摄影师发送的选片、审片或交付链接。'
    return
  }
  url.value = value
})

onUnload(() => {
  if (typeof uni.offUserCaptureScreen === 'function') uni.offUserCaptureScreen(onCapture)
  screenBridge()?.offScreenRecordingStateChanged?.(onRecordingChange)
  if (captureTimer) clearTimeout(captureTimer)
})
</script>

<template>
  <web-view v-if="url" :src="url" />
  <cover-view v-if="captureGuard" class="capture-guard"><cover-view>内容已遮挡</cover-view></cover-view>
  <view v-else class="client-page"><view class="notice">{{ error }}</view></view>
</template>
