<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { CreatedProofRound } from '../api/proof'
import { createProofRound, listProofRounds } from '../api/proof'
import { toast } from '../state/toast'
import { copyText } from '../utils/clipboard'
import { fromLocalInput, toLocalInput } from '../utils/format'

const props = defineProps<{
  open: boolean
  projectId: string
  galleryId: string
  galleryTitle: string
  selections: { asset_id: string; filename: string }[]
}>()
const emit = defineEmits<{ close: []; created: [id: string] }>()

const dueAt = ref(defaultDue())
const selectedAssets = ref<Set<string>>(new Set())
const roundsTotal = ref(0)
const loadingMeta = ref(false)
const metaError = ref('')
const submitting = ref(false)
const error = ref('')
const issued = ref<CreatedProofRound | null>(null)

const nextRound = computed(() => roundsTotal.value + 1)
const allSelected = computed(
  () => props.selections.length > 0 && selectedAssets.value.size === props.selections.length,
)

function defaultDue(): string {
  const date = new Date()
  date.setDate(date.getDate() + 7)
  date.setHours(21, 0, 0, 0)
  return toLocalInput(date.toISOString())
}

function reset(): void {
  dueAt.value = defaultDue()
  selectedAssets.value = new Set(props.selections.map((s) => s.asset_id))
  roundsTotal.value = 0
  error.value = ''
  metaError.value = ''
  issued.value = null
}

async function loadMeta(): Promise<void> {
  loadingMeta.value = true
  metaError.value = ''
  try {
    const rounds = await listProofRounds({ project_id: props.projectId, limit: 1 })
    roundsTotal.value = rounds.total
  } catch (err) {
    metaError.value = err instanceof ApiError ? err.detail || err.message : '加载项目信息失败'
  } finally {
    loadingMeta.value = false
  }
}

function toggleAsset(id: string): void {
  const next = new Set(selectedAssets.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selectedAssets.value = next
}

function toggleAll(): void {
  selectedAssets.value = allSelected.value
    ? new Set()
    : new Set(props.selections.map((s) => s.asset_id))
}

async function submit(): Promise<void> {
  if (submitting.value) return
  error.value = ''
  if (selectedAssets.value.size === 0) {
    error.value = '请至少选择一张需要审片的照片。'
    return
  }
  const due = fromLocalInput(dueAt.value)
  if (!due) {
    error.value = '请选择有效的截止时间。'
    return
  }
  const body: Parameters<typeof createProofRound>[0] = {
    project_id: props.projectId,
    gallery_id: props.galleryId,
    due_at: due,
    asset_ids:
      selectedAssets.value.size === props.selections.length ? [] : [...selectedAssets.value],
  }
  submitting.value = true
  try {
    issued.value = await createProofRound(body)
    toast('审片轮次已创建，客户邮件已加入发送队列', 'success')
  } catch (err) {
    error.value =
      err instanceof ApiError
        ? err.detail || err.message
        : '创建审片失败，请稍后重试'
    if (err instanceof ApiError && err.code === 'derivatives_incomplete') {
      error.value = `${err.detail}（预览图已排队生成，稍后重试即可）`
    }
  } finally {
    submitting.value = false
  }
}

async function copy(value: string): Promise<void> {
  const ok = await copyText(value)
  toast(ok ? '链接已复制' : '复制失败，请手动选择文本复制', ok ? 'success' : 'error')
}

watch(
  () => props.open,
  (open) => {
    if (!open) return
    reset()
    void loadMeta()
  },
  { immediate: true },
)

watch(
  () => props.selections,
  () => {
    if (props.open) selectedAssets.value = new Set(props.selections.map((s) => s.asset_id))
  },
)
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay" @click.self="!submitting && emit('close')">
      <div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="发起审片">
        <template v-if="issued">
          <h2 class="dialog-title">审片轮次已创建</h2>
          <p class="muted">
            {{ galleryTitle }} · 第 {{ issued.round.round_no }} 轮 · {{ issued.items }} 张照片。
          </p>
          <label class="field">
            <span>客户审片链接（仅显示一次）</span>
            <div class="copy-row">
              <input :value="issued.token.proof_url" readonly @focus="($event.target as HTMLInputElement).select()" />
              <button class="btn sm" type="button" @click="copy(issued.token.proof_url)">复制</button>
            </div>
          </label>
          <p class="muted small">
            邀请邮件已加入发送队列；链接只能查看这一轮，撤销相册或令牌后立即失效。
          </p>
          <div class="dialog-actions">
            <button class="btn primary" type="button" @click="emit('created', issued!.round.id)">
              我已保存，进入审片
            </button>
          </div>
        </template>

        <template v-else>
          <h2 class="dialog-title">发起审片</h2>
          <p class="muted small">{{ galleryTitle }} · 从客户已提交的选片发起一轮审片。</p>

          <p v-if="metaError" class="alert error" role="alert">{{ metaError }}</p>
          <p v-else-if="loadingMeta" class="muted small">正在读取审片轮次…</p>
          <p v-else class="muted small">这是第 {{ nextRound }} 轮审片，可重复提交修改直至客户通过。</p>

          <label class="field">
            <span>审片截止时间</span>
            <input v-model="dueAt" type="datetime-local" />
          </label>

          <div class="field">
            <span>
              审片范围（已选 {{ selectedAssets.size }} / {{ selections.length }} 张，超过 500 张需分批）
            </span>
            <div class="gate-actions">
              <button class="btn ghost sm" type="button" @click="toggleAll">
                {{ allSelected ? '全部取消' : '全选' }}
              </button>
            </div>
            <div class="asset-checklist">
              <label v-for="asset in selections" :key="asset.asset_id" class="check-row">
                <input
                  type="checkbox"
                  :checked="selectedAssets.has(asset.asset_id)"
                  @change="toggleAsset(asset.asset_id)"
                />
                <span>{{ asset.filename }}</span>
              </label>
              <p v-if="selections.length === 0" class="muted small">相册还没有客户已选照片。</p>
            </div>
          </div>

          <p v-if="error" class="form-error" role="alert">{{ error }}</p>

          <div class="dialog-actions">
            <button class="btn ghost" type="button" :disabled="submitting" @click="emit('close')">取消</button>
            <button
              class="btn primary"
              type="button"
              :disabled="submitting || loadingMeta || selections.length === 0"
              @click="submit"
            >
              {{ submitting ? '创建中…' : '创建审片并发邀请邮件' }}
            </button>
          </div>
        </template>
      </div>
    </div>
  </Teleport>
</template>
