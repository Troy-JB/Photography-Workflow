<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { Gallery, Project } from '../api/types'
import { api } from '../state/session'
import { toast } from '../state/toast'
import { fromLocalInput, toLocalInput } from '../utils/format'
import { parseNumberInput } from '../utils/number'
import { ApiError } from '../api/client'

const props = defineProps<{ open: boolean; projects: Project[]; defaultProject?: string }>()
const emit = defineEmits<{ close: []; created: [gallery: Gallery] }>()

const projectID = ref('')
const title = ref('')
const expiresAt = ref('')
const limit = ref('')
const watermark = ref('')
const download = ref(false)
const busy = ref(false)
const error = ref('')

function reset(): void {
  projectID.value = props.defaultProject || props.projects[0]?.id || ''
  title.value = ''
  expiresAt.value = toLocalInput(new Date(Date.now() + 7 * 86_400_000).toISOString())
  limit.value = ''
  watermark.value = ''
  download.value = false
  error.value = ''
}

watch(
  () => props.open,
  (open) => {
    if (open) reset()
  },
)

const canSubmit = computed(() => Boolean(projectID.value && title.value.trim()) && !busy.value)

async function submit(): Promise<void> {
  if (!canSubmit.value) return
  busy.value = true
  error.value = ''
  // v-model on type="number" yields a number, so normalize before trimming.
  const parsedLimit = parseNumberInput(limit.value)
  const limitValue = parsedLimit.empty ? null : parsedLimit.value
  if (limitValue !== null && (!Number.isInteger(limitValue) || limitValue <= 0)) {
    error.value = '选片上限必须是正整数，或留空表示不限'
    busy.value = false
    return
  }
  try {
    const gallery = await api<Gallery>('/api/v1/galleries', {
      method: 'POST',
      body: {
        project_id: projectID.value,
        title: title.value.trim(),
        expires_at: fromLocalInput(expiresAt.value),
        selection_limit: limitValue,
        download_allowed: download.value,
        watermark_text: watermark.value.trim(),
      },
    })
    toast('相册已创建', 'success')
    emit('created', gallery)
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.code : '创建失败，请稍后重试'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay" @click.self="!busy && emit('close')">
      <div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="新建相册">
        <h2 class="dialog-title">新建选片相册</h2>
        <form class="form-grid" @submit.prevent="submit">
          <label class="field span-2">
            <span>项目</span>
            <select v-model="projectID" :disabled="busy">
              <option v-for="project in projects" :key="project.id" :value="project.id">
                {{ project.code }} · {{ project.title }}
              </option>
            </select>
          </label>
          <label class="field span-2">
            <span>相册标题（客户可见）</span>
            <input v-model="title" type="text" maxlength="120" placeholder="例如：婚礼精修第一轮" :disabled="busy" />
          </label>
          <label class="field">
            <span>选片截止时间</span>
            <input v-model="expiresAt" type="datetime-local" :disabled="busy" />
          </label>
          <label class="field">
            <span>选片上限（张，留空不限）</span>
            <input v-model="limit" type="number" min="1" step="1" placeholder="不限" :disabled="busy" />
          </label>
          <label class="field">
            <span>水印文字（留空使用 Lumeo）</span>
            <input v-model="watermark" type="text" maxlength="120" placeholder="工作室名称" :disabled="busy" />
          </label>
          <label class="field checkbox-field">
            <input v-model="download" type="checkbox" :disabled="busy" />
            <span>允许下载（下载入口在交付阶段开放）</span>
          </label>

          <p v-if="error" class="form-error span-2" role="alert">{{ error }}</p>

          <div class="dialog-actions span-2">
            <button class="btn ghost" type="button" :disabled="busy" @click="emit('close')">取消</button>
            <button class="btn primary" type="submit" :disabled="!canSubmit">
              {{ busy ? '创建中…' : '创建相册' }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </Teleport>
</template>
