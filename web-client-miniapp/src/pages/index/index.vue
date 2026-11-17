<script setup lang="ts">
import { onShow } from '@dcloudio/uni-app'
import { ref } from 'vue'
import { getSession, request } from '../../api'

const session = ref(getSession())
const projects = ref<{ id: string; title: string; state: string; studio_name: string; shoot_start_at?: string }[]>([])
const loading = ref(false)
const error = ref('')
const inviteInput = ref('')

function extractInviteToken(value: string): string {
  const raw = value.trim()
  const match = raw.match(/[?&](?:token|scene)=([^&#]+)/) ?? raw.match(/(?:token|scene)=([^&#]+)/)
  return decodeURIComponent(match?.[1] ?? raw)
}

function projectStateLabel(state: string): string {
  const labels: Record<string, string> = {
    draft: '待沟通', quoted: '报价中', contract_pending: '待签约', contracted: '已签约',
    booked: '已预约', shooting: '拍摄中', post_production: '后期制作', completed: '已完成',
    archived: '已归档', on_hold: '暂停', cancelled: '已取消',
  }
  return labels[state] ?? state
}

async function load(): Promise<void> {
  session.value = getSession()
  if (!session.value) return
  loading.value = true
  error.value = ''
  try {
    const result = await request<{ items: typeof projects.value }>('/c/v1/me/projects', 'GET', undefined, true)
    projects.value = result.items ?? []
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '暂时无法加载拍摄项目'
  } finally {
    loading.value = false
  }
}

function openInvite(): void {
  const raw = inviteInput.value.trim()
  const token = extractInviteToken(raw)
  if (!raw) {
    error.value = '请输入摄影师编号'
    return
  }
  const isCode = /^[a-z0-9-]{6,24}$/i.test(raw) && !/[?&=]/.test(raw)
  uni.navigateTo({ url: `/pages/invite/invite?${isCode ? 'code' : 'token'}=${encodeURIComponent(isCode ? raw.toUpperCase() : token)}` })
}

function openProjects(): void {
  uni.switchTab({ url: '/pages/projects/projects' })
}

onShow(() => void load())
</script>

<template>
  <view class="client-page">
    <view class="client-brand"><text class="brand-name">STUDIO / CLIENT</text><text>客户中心</text></view>
    <view class="client-title">把拍摄时间，<br />留给值得的瞬间。</view>
    <view class="client-subtitle">预约档期 · 查看项目进度</view>

    <view class="client-section">
      <view class="client-section-head"><text class="section-name">预约摄影师</text><text>输入个人编号</text></view>
      <input v-model="inviteInput" class="invite-input" placeholder="例如 PH12AB34CD56" />
      <button class="primary-button" @click="openInvite">查看服务与档期</button>
    </view>

    <view v-if="session" class="client-section">
      <view class="client-section-head"><text class="section-name">最近拍摄</text><text @click="openProjects">查看全部 ›</text></view>
      <view v-if="loading" class="empty-state">加载中…</view>
      <view v-else-if="error" class="notice">{{ error }}</view>
      <view v-else-if="!projects.length" class="empty-state">预约后，拍摄项目会显示在这里。</view>
      <view v-for="project in projects.slice(0, 3)" :key="project.id" class="project-row" @click="openProjects">
        <text class="project-title">{{ project.title }}</text><text>{{ project.studio_name }} · {{ projectStateLabel(project.state) }}<template v-if="project.shoot_start_at"> · {{ project.shoot_start_at.slice(0, 10) }}</template></text>
      </view>
    </view>
    <view v-if="session" class="client-section notice">已登录 {{ session?.customer.phone }}。预约后可在“我的拍摄”查看项目及最新进度。</view>
  </view>
</template>
