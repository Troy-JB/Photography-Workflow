<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { ProofRoundSummary } from '../api/proof'
import { listProofRounds } from '../api/proof'
import type { ListResponse, Project } from '../api/types'
import PagerBar from '../components/PagerBar.vue'
import IntegrationPanel from '../components/IntegrationPanel.vue'
import { api } from '../state/session'
import { deadlineLabel, formatDateTime, proofStatusLabel, proofStatusTone } from '../utils/format'

const router = useRouter()
const route = useRoute()

const projects = ref<Project[]>([])
const projectCatalog = ref<Project[]>([])
const rounds = ref<ProofRoundSummary[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 20
const projectFilter = ref(typeof route.query.project === 'string' ? route.query.project : '')
const statusFilter = ref('')
const loading = ref(false)
const error = ref('')

const projectNames = computed(() => {
  const map = new Map<string, string>()
  for (const project of projectCatalog.value) map.set(project.id, `${project.code} · ${project.title}${project.state === 'cancelled' ? ' · 已取消' : project.state === 'archived' ? ' · 已归档' : ''}`)
  return map
})

function projectName(id: string): string {
  return projectNames.value.get(id) ?? id.slice(0, 8)
}

async function loadProjects(): Promise<void> {
  try {
    const [all, selectable] = await Promise.all([
      api<ListResponse<Project>>('/api/v1/projects?limit=200'),
      api<ListResponse<Project>>('/api/v1/projects?limit=200&exclude_closed=true'),
    ])
    projectCatalog.value = all.items
    projects.value = selectable.items
  } catch {
    // Filters degrade to raw ids; the list itself is unaffected.
  }
}

async function loadRounds(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const page = await listProofRounds({
      project_id: projectFilter.value || undefined,
      status: statusFilter.value || undefined,
      limit,
      offset: offset.value,
    })
    rounds.value = page.items
    total.value = page.total
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载审片轮次失败'
  } finally {
    loading.value = false
  }
}

watch([projectFilter, statusFilter], () => {
  offset.value = 0
  void loadRounds()
})

function openRound(round: ProofRoundSummary): void {
  void router.push({ name: 'proof', params: { id: round.id } })
}

onMounted(async () => {
  await Promise.all([loadProjects(), loadRounds()])
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1>精修相册</h1>
        <p class="muted">管理精修审片、修改意见、修图回传与最终交付。</p>
      </div>
      <div class="row-actions"><RouterLink to="/galleries" class="btn ghost">从原片相册发起</RouterLink><RouterLink to="/deliveries" class="btn ghost">交付列表</RouterLink></div>
    </header>

    <div class="toolbar">
      <label class="inline-field">
        <span>项目</span>
        <select v-model="projectFilter">
          <option value="">全部项目</option>
          <option v-for="project in projects" :key="project.id" :value="project.id">
            {{ project.code }} · {{ project.title }}
          </option>
        </select>
      </label>
      <label class="inline-field">
        <span>状态</span>
        <select v-model="statusFilter">
          <option value="">全部状态</option>
          <option value="open">待客户查看</option>
          <option value="in_review">客户审阅中</option>
          <option value="changes_requested">客户提出修改</option>
          <option value="retouching">修图中</option>
          <option value="resubmitted">已回传待确认</option>
          <option value="approved">已通过</option>
        </select>
      </label>
      <button class="btn ghost" type="button" :disabled="loading" @click="loadRounds">刷新</button>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="panel">
      <table class="data">
        <thead>
          <tr>
            <th>轮次</th>
            <th>项目</th>
            <th>状态</th>
            <th class="num">照片</th>
            <th class="num">未解决批注</th>
            <th>截止</th>
            <th>创建时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="round in rounds" :key="round.id" class="clickable" @click="openRound(round)">
            <td>
              <strong>第 {{ round.round_no }} 轮</strong>
              <div class="muted small">{{ round.id.slice(0, 8) }}</div>
            </td>
            <td class="muted">{{ projectName(round.project_id) }}</td>
            <td>
              <span class="pill" :class="proofStatusTone(round.status)">{{ proofStatusLabel(round.status) }}</span>
              <span v-if="round.override_reason" class="pill danger" title="owner 强制放行">override</span>
            </td>
            <td class="num">{{ round.asset_count }}</td>
            <td class="num">
              <span :class="{ 'accent-text': round.open_comments > 0 }">{{ round.open_comments }}</span>
              <span class="muted"> / {{ round.comment_count }}</span>
            </td>
            <td>{{ deadlineLabel(round.due_at) }}</td>
            <td>{{ formatDateTime(round.created_at) }}</td>
            <td class="row-action">
              <button class="btn ghost sm" type="button" @click.stop="openRound(round)">查看</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="loading" class="empty muted">加载中…</p>
      <p v-else-if="rounds.length === 0" class="empty muted">
        还没有审片轮次。打开一个已提交选片的相册，点击「发起审片」。
      </p>
    </div>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; loadRounds() }"
    />

    <IntegrationPanel />
  </section>
</template>
