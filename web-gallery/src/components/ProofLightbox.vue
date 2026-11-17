<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { Annotation, AnnotationPoint, AnnotationType, ProofItem } from '../api/proof'
import {
  ANNOTATION_COLORS,
  annotationLabel,
  annotationTypes,
  dragAnnotation,
  freehandAnnotation,
  normalizePoint,
  pointAnnotation,
} from '../proof/annotation'
import type { CommentStore, CommentThread } from '../proof/comments'
import { isProofTerminalError } from '../state/proof'
import { requestKey } from '../utils/keys'
import { prefetchImage } from '../utils/lazy'
import AnnotationShapes from './AnnotationShapes.vue'

const props = defineProps<{
  items: ProofItem[]
  index: number
  comments: CommentStore
  canComment: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'update:index', index: number): void
  (e: 'stale'): void
  (e: 'fatal', error: unknown): void
  (e: 'notify', message: string, kind: 'info' | 'error' | 'success'): void
}>()

const PALETTE = ANNOTATION_COLORS
const tools = annotationTypes()

const item = computed<ProofItem | null>(() => props.items[props.index] ?? null)
const canPrev = computed(() => props.index > 0)
const canNext = computed(() => props.index < props.items.length - 1)
const draftColor = computed(() => PALETTE[props.index % PALETTE.length])

const closeRef = ref<HTMLButtonElement | null>(null)
const canvasRef = ref<HTMLElement | null>(null)
const panelRef = ref<HTMLElement | null>(null)
const loaded = ref(false)
const failed = ref(false)

const tool = ref<AnnotationType | null>(null)
const draft = ref<Annotation | null>(null)
const activeID = ref('')

const body = ref('')
const composerKey = ref(requestKey('proof-comment'))
const composerError = ref('')
const sending = ref(false)

const replyTo = ref('')
const replyBody = ref('')
const replyKey = ref('')
const replyError = ref('')
const replySending = ref(false)

let drawing = false
const dragStart = ref<AnnotationPoint | null>(null)
let freehandPoints: AnnotationPoint[] = []
const dragPoint = ref<AnnotationPoint | null>(null)
const freehandLive = ref<AnnotationPoint[]>([])

const threads = computed<CommentThread[]>(() =>
  item.value ? props.comments.threadsFor(item.value.asset_id) : [],
)
const openThreads = computed(() => threads.value.filter((thread) => !thread.resolved).length)

const visibleComments = computed(() =>
  item.value
    ? props.comments.state.items.filter((entry) => entry.asset_id === item.value?.asset_id)
    : [],
)

const annotationEntries = computed(() =>
  visibleComments.value
    .filter((entry) => entry.annotation)
    .map((entry) => ({ id: entry.id, annotation: entry.annotation as Annotation })),
)

const liveDraft = computed<Annotation | null>(() => {
  if (!drawing) return null
  if (tool.value === 'freehand') return freehandAnnotation(freehandLive.value, draftColor.value)
  if ((tool.value === 'rect' || tool.value === 'ellipse') && dragStart.value && dragPoint.value) {
    return dragAnnotation(tool.value, dragStart.value, dragPoint.value, draftColor.value)
  }
  return null
})

const previewAnnotation = computed(() => draft.value ?? liveDraft.value)

const placeholderStyle = computed(() => {
  if (item.value?.lqip) return { backgroundImage: `url("${item.value.lqip}")` }
  if (item.value?.color) return { backgroundColor: item.value.color }
  return undefined
})

const canSend = computed(
  () => props.canComment && !sending.value && (body.value.trim().length > 0 || draft.value !== null),
)

const toolTip = computed(() => {
  switch (tool.value) {
    case 'point':
      return '点按照片放置标记'
    case 'rect':
    case 'ellipse':
      return '在照片上拖动画出范围'
    case 'freehand':
      return '在照片上拖动自由手绘'
    default:
      return ''
  }
})

const draftLabel = computed(() => annotationLabel(previewAnnotation.value))

watch(
  () => props.index,
  () => {
    loaded.value = false
    failed.value = false
    resetLocal()
    const before = props.items[props.index - 1]
    const after = props.items[props.index + 1]
    if (before) prefetchImage(before.display_url)
    if (after) prefetchImage(after.display_url)
  },
  { immediate: true },
)

watch(
  () => item.value?.display_url,
  () => {
    failed.value = false
    loaded.value = false
  },
)

function resetLocal() {
  tool.value = null
  draft.value = null
  activeID.value = ''
  body.value = ''
  composerError.value = ''
  composerKey.value = requestKey('proof-comment')
  replyTo.value = ''
  replyBody.value = ''
  replyError.value = ''
  drawing = false
  dragStart.value = null
  dragPoint.value = null
  freehandPoints = []
  freehandLive.value = []
}

function go(delta: number) {
  const next = props.index + delta
  if (next < 0 || next >= props.items.length) return
  emit('update:index', next)
}

function toggleTool(next: AnnotationType) {
  if (!props.canComment) return
  tool.value = tool.value === next ? null : next
  draft.value = null
  freehandLive.value = []
  drawing = false
}

function clearDraft() {
  draft.value = null
  tool.value = null
}

function focusComposer() {
  tool.value = null
  draft.value = null
  nextTick(() => panelRef.value?.scrollTo({ top: panelRef.value.scrollHeight, behavior: 'smooth' }))
}

function onError() {
  loaded.value = false
  if (failed.value) return
  failed.value = true
  emit('stale')
}

/* ---------- drawing ---------- */

function pointerPoint(event: PointerEvent): AnnotationPoint | null {
  const canvas = canvasRef.value
  if (!canvas) return null
  const rect = canvas.getBoundingClientRect()
  return normalizePoint(event.clientX, event.clientY, rect)
}

function onCanvasDown(event: PointerEvent) {
  if (!tool.value || !props.canComment) return
  event.preventDefault()
  event.stopPropagation()
  const point = pointerPoint(event)
  if (!point) return
  if (tool.value === 'point') {
    finishDraft(pointAnnotation(point, draftColor.value))
    return
  }
  drawing = true
  dragStart.value = point
  dragPoint.value = point
  freehandPoints = [point]
  freehandLive.value = [point]
  ;(event.currentTarget as Element).setPointerCapture?.(event.pointerId)
}

function onCanvasMove(event: PointerEvent) {
  if (!drawing) return
  event.preventDefault()
  const point = pointerPoint(event)
  if (!point) return
  dragPoint.value = point
  if (tool.value === 'freehand') {
    freehandPoints.push(point)
    freehandLive.value = [...freehandPoints]
  }
}

function onCanvasUp(event: PointerEvent) {
  if (!drawing) return
  drawing = false
  ;(event.currentTarget as Element).releasePointerCapture?.(event.pointerId)
  const finish = pointerPoint(event)
  if (tool.value === 'freehand') {
    const annotation = freehandAnnotation(
      finish ? [...freehandPoints, finish] : freehandPoints,
      draftColor.value,
    )
    freehandLive.value = []
    if (annotation) finishDraft(annotation)
    else emit('notify', '手绘范围太小，请重新绘制', 'info')
    return
  }
  if ((tool.value === 'rect' || tool.value === 'ellipse') && dragStart.value && finish) {
    const annotation = dragAnnotation(tool.value, dragStart.value, finish, draftColor.value)
    if (annotation) finishDraft(annotation)
    else emit('notify', '绘制范围太小，请重新绘制', 'info')
  }
  dragStart.value = null
  dragPoint.value = null
}

function finishDraft(annotation: Annotation) {
  draft.value = annotation
  tool.value = null
  freehandLive.value = []
  void nextTick(() => panelRef.value?.scrollTo({ top: panelRef.value.scrollHeight, behavior: 'smooth' }))
}

/* ---------- swipe navigation ---------- */

let swipeStartX = 0
let swipeStartY = 0
let swiping = false

function onStageDown(event: PointerEvent) {
  if (tool.value || drawing) return
  swiping = true
  swipeStartX = event.clientX
  swipeStartY = event.clientY
}

function onStageUp(event: PointerEvent) {
  if (!swiping) return
  swiping = false
  const dx = event.clientX - swipeStartX
  const dy = event.clientY - swipeStartY
  if (Math.abs(dx) < 48 || Math.abs(dx) < Math.abs(dy)) return
  go(dx > 0 ? -1 : 1)
}

/* ---------- comments ---------- */

function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(
    new Date(iso),
  )
}

function canResolve(thread: CommentThread): boolean {
  return (
    props.canComment &&
    !thread.resolved &&
    thread.root.author_type === 'client' &&
    !thread.root.id.startsWith('tmp:')
  )
}

async function resolveThread(id: string) {
  try {
    await props.comments.resolve(id)
    emit('notify', '已标记为已解决', 'success')
  } catch (error) {
    if (isProofTerminalError(error)) {
      emit('fatal', error)
      return
    }
    emit('notify', describe(error), 'error')
  }
}

function startReply(thread: CommentThread) {
  if (!props.canComment) return
  replyTo.value = replyTo.value === thread.root.id ? '' : thread.root.id
  replyBody.value = ''
  replyError.value = ''
  replyKey.value = requestKey('proof-reply')
  activeID.value = thread.root.id
}

async function sendReply() {
  const current = item.value
  const rootID = replyTo.value
  const text = replyBody.value.trim()
  if (!current || !rootID || !text || replySending.value) return
  replySending.value = true
  replyError.value = ''
  try {
    const created = await props.comments.create(
      { asset_id: current.asset_id, parent_id: rootID, body: text },
      replyKey.value,
    )
    replyTo.value = ''
    replyBody.value = ''
    replyKey.value = requestKey('proof-reply')
    activeID.value = created.parent_id ?? created.id
  } catch (error) {
    if (isProofTerminalError(error)) {
      emit('fatal', error)
      return
    }
    replyError.value = describe(error)
  } finally {
    replySending.value = false
  }
}

async function sendRoot() {
  const current = item.value
  if (!current || !canSend.value) return
  sending.value = true
  composerError.value = ''
  const input = {
    asset_id: current.asset_id,
    body: body.value.trim(),
    annotation: draft.value ?? undefined,
  }
  try {
    const created = await props.comments.create(input, composerKey.value)
    resetLocal()
    activeID.value = created.id
    emit('notify', '批注已保存', 'success')
  } catch (error) {
    if (isProofTerminalError(error)) {
      emit('fatal', error)
      return
    }
    composerError.value = describe(error)
  } finally {
    sending.value = false
  }
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.detail || error.code
  if (error instanceof Error) return error.message
  return '网络异常，请稍后重试'
}

/* ---------- keyboard ---------- */

function onKeydown(event: KeyboardEvent) {
  switch (event.key) {
    case 'Escape':
      event.preventDefault()
      if (tool.value) {
        tool.value = null
        return
      }
      emit('close')
      break
    case 'ArrowLeft':
      if (event.target instanceof HTMLElement && event.target.closest('textarea, input')) return
      event.preventDefault()
      go(-1)
      break
    case 'ArrowRight':
      if (event.target instanceof HTMLElement && event.target.closest('textarea, input')) return
      event.preventDefault()
      go(1)
      break
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  closeRef.value?.focus()
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <Teleport to="body">
    <div class="proof-lb" role="dialog" aria-modal="true" aria-label="审片查看">
      <div class="proof-lb-head">
        <button ref="closeRef" type="button" class="lb-close" aria-label="关闭" @click="emit('close')">
          ✕
        </button>
        <span class="proof-lb-title">{{ item?.filename ?? '' }}</span>
        <span v-if="item && item.revision_no > 0" class="proof-rev-chip">修图版 v{{ item.revision_no }}</span>
        <span class="proof-lb-counter">{{ index + 1 }} / {{ items.length }}</span>
        <div class="proof-lb-nav">
          <button type="button" class="lb-nav-btn" :disabled="!canPrev" aria-label="上一张" @click="go(-1)">‹</button>
          <button type="button" class="lb-nav-btn" :disabled="!canNext" aria-label="下一张" @click="go(1)">›</button>
        </div>
      </div>

      <div class="proof-lb-body">
        <div class="proof-stage" @pointerdown="onStageDown" @pointerup="onStageUp">
          <span v-if="item && !loaded" class="lb-placeholder" :style="placeholderStyle" />
          <div
            ref="canvasRef"
            class="proof-canvas"
            :class="{ 'is-drawing': tool !== null }"
            @pointerdown="onCanvasDown"
            @pointermove="onCanvasMove"
            @pointerup="onCanvasUp"
            @pointercancel="onCanvasUp"
          >
            <img
              v-if="item"
              :key="item.asset_id"
              :src="item.display_url"
              class="proof-image"
              :class="{ 'is-hidden': !loaded }"
              :alt="item.filename"
              decoding="async"
              draggable="false"
              @load="loaded = true"
              @error="onError"
            />
            <AnnotationShapes
              v-if="item"
              :annotations="annotationEntries"
              :width="item.width"
              :height="item.height"
              :active-id="activeID"
            />
            <AnnotationShapes
              v-if="item && previewAnnotation"
              :annotations="[{ id: 'draft', annotation: previewAnnotation }]"
              :width="item.width"
              :height="item.height"
              active-id="draft"
            />
          </div>
          <span v-if="item && !loaded && !failed" class="lb-spinner">加载中…</span>
          <span v-if="failed" class="lb-spinner">图片加载失败，正在重新获取…</span>
          <span v-if="item?.is_video" class="lb-note">视频将于交付阶段提供，这里显示封面帧</span>

          <div v-if="canComment" class="proof-tools" role="toolbar" aria-label="批注工具">
            <button
              v-for="entry in tools"
              :key="entry.key"
              type="button"
              class="proof-tool"
              :class="{ 'is-active': tool === entry.key }"
              :aria-pressed="tool === entry.key"
              @click="toggleTool(entry.key)"
            >
              {{ entry.label }}
            </button>
            <button type="button" class="proof-tool is-text" @click="focusComposer">写评论</button>
          </div>
          <p v-if="tool" class="proof-tool-tip">{{ toolTip }}，完成后填写说明。</p>
        </div>

        <aside ref="panelRef" class="proof-panel">
          <div class="proof-panel-head">
            <span>批注</span>
            <span class="muted small">{{ threads.length }} 条 · {{ openThreads }} 条待处理</span>
          </div>

          <div v-if="comments.state.loading && !comments.state.loaded" class="proof-threads-empty">
            <span class="spinner" /> 正在加载批注…
          </div>
          <div v-else-if="!threads.length" class="proof-threads-empty">
            还没有批注。使用上方工具在照片上标注，或直接写评论。
          </div>

          <div v-else class="proof-threads">
            <article
              v-for="thread in threads"
              :key="thread.root.id"
              class="proof-thread"
              :class="{ 'is-resolved': thread.resolved, 'is-active': activeID === thread.root.id }"
              @click="activeID = thread.root.id"
            >
              <header class="proof-thread-head">
                <span class="proof-who">{{ thread.root.author_type === 'client' ? '您' : '工作室' }}</span>
                <span class="muted small">{{ formatTime(thread.root.created_at) }}</span>
                <span v-if="thread.root.annotation" class="proof-chip">{{ annotationLabel(thread.root.annotation) }}</span>
                <span v-if="thread.resolved" class="proof-chip is-ok">已解决</span>
              </header>
              <p v-if="thread.root.body" class="proof-thread-body">{{ thread.root.body }}</p>
              <p v-else class="proof-thread-body muted">（仅批注）</p>

              <div v-for="reply in thread.replies" :key="reply.id" class="proof-reply">
                <header class="proof-thread-head">
                  <span class="proof-who">{{ reply.author_type === 'client' ? '您' : '工作室' }}</span>
                  <span class="muted small">{{ formatTime(reply.created_at) }}</span>
                </header>
                <p class="proof-thread-body">{{ reply.body }}</p>
              </div>

              <div class="proof-thread-actions">
                <button v-if="canComment" type="button" class="proof-link" @click.stop="startReply(thread)">
                  {{ replyTo === thread.root.id ? '收起回复' : '回复' }}
                </button>
                <button v-if="canResolve(thread)" type="button" class="proof-link" @click.stop="resolveThread(thread.root.id)">
                  标记已解决
                </button>
              </div>

              <form v-if="replyTo === thread.root.id" class="proof-reply-form" @submit.prevent="sendReply" @click.stop>
                <textarea
                  v-model="replyBody"
                  rows="2"
                  maxlength="2000"
                  placeholder="回复这条批注…"
                  :disabled="replySending"
                />
                <div class="proof-reply-row">
                  <span v-if="replyError" class="proof-error">{{ replyError }}</span>
                  <button type="submit" class="btn btn-primary btn-small" :disabled="replySending || !replyBody.trim()">
                    {{ replySending ? '发送中…' : '发送回复' }}
                  </button>
                </div>
              </form>
            </article>
          </div>

          <div v-if="canComment" class="proof-composer">
            <div v-if="previewAnnotation" class="proof-draft">
              <span>{{ draftLabel }}</span>
              <button v-if="draft" type="button" class="proof-link" @click="clearDraft">移除</button>
            </div>
            <textarea
              v-model="body"
              rows="2"
              maxlength="2000"
              placeholder="补充说明（可选）"
              :disabled="sending"
            />
            <div class="proof-composer-row">
              <span class="muted small">{{ body.length }} / 2000</span>
              <button type="button" class="btn btn-primary btn-small" :disabled="!canSend" @click="sendRoot">
                {{ sending ? '发送中…' : '发送批注' }}
              </button>
            </div>
            <p v-if="composerError" class="proof-error">{{ composerError }}</p>
          </div>
          <p v-else class="proof-readonly">本轮已通过，批注为只读。</p>
        </aside>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.proof-lb {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: flex;
  flex-direction: column;
  background: #050608;
  --bg: #050608;
  --bg-soft: #121316;
  --panel: #1b1d22;
  --line: #353941;
  --line-strong: #555b65;
  --text: #fff8ef;
  --muted: #c4b2a4;
  --accent: #d98860;
  --accent-strong: #f0a36f;
  --accent-ink: #281811;
  --danger: #e17a6b;
  --ok: #78b99a;
  padding-top: env(safe-area-inset-top);
}

.proof-lb-head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border-bottom: 1px solid var(--line);
  color: var(--muted);
  font-size: 12.5px;
}

.proof-lb-title {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text);
}

.proof-lb-counter {
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.proof-lb-nav {
  display: flex;
  gap: 6px;
}

.lb-nav-btn {
  width: 32px;
  height: 32px;
  border-radius: 50%;
  border: 1px solid var(--line);
  display: grid;
  place-items: center;
  font-size: 16px;
}

.proof-lb-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}

.proof-stage {
  position: relative;
  flex: 1;
  min-height: 0;
  display: grid;
  place-items: center;
  padding: 8px 10px;
  overflow: hidden;
  touch-action: pan-y;
}

.proof-canvas {
  position: relative;
  display: flex;
  max-width: 100%;
  max-height: 100%;
  touch-action: pan-y;
}

.proof-canvas.is-drawing {
  touch-action: none;
  cursor: crosshair;
}

.proof-image {
  display: block;
  max-width: 100%;
  max-height: 100%;
  width: auto;
  height: auto;
  object-fit: contain;
  user-select: none;
  -webkit-user-drag: none;
  transition: opacity 0.2s ease;
}

.proof-image.is-hidden {
  opacity: 0;
}

.proof-canvas :deep(.annotation-layer) {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
  overflow: visible;
}

.proof-tools {
  position: absolute;
  left: 50%;
  bottom: 10px;
  transform: translateX(-50%);
  display: flex;
  gap: 6px;
  padding: 6px;
  border-radius: 999px;
  background: rgb(12 14 18 / 88%);
  border: 1px solid var(--line);
  backdrop-filter: blur(8px);
  z-index: 2;
}

.proof-tool {
  padding: 6px 12px;
  border-radius: 999px;
  font-size: 12.5px;
  color: var(--muted);
  white-space: nowrap;
}

.proof-tool.is-active {
  background: var(--accent);
  color: #221a08;
  font-weight: 600;
}

.proof-tool.is-text {
  border-left: 1px solid var(--line);
  border-radius: 0 999px 999px 0;
  padding-left: 14px;
}

.proof-tool-tip {
  position: absolute;
  top: 10px;
  left: 50%;
  transform: translateX(-50%);
  margin: 0;
  padding: 5px 12px;
  border-radius: 999px;
  background: rgb(9 10 13 / 78%);
  color: #f2d9a6;
  font-size: 11.5px;
  white-space: nowrap;
  z-index: 2;
}

.proof-panel {
  flex: none;
  max-height: 46dvh;
  overflow-y: auto;
  padding: 12px 14px calc(14px + env(safe-area-inset-bottom));
  border-top: 1px solid var(--line);
  background: var(--bg-soft);
  overscroll-behavior: contain;
}

.proof-panel-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
  font-size: 13.5px;
  font-weight: 600;
  margin-bottom: 10px;
}

.proof-threads-empty {
  color: var(--muted);
  font-size: 12.5px;
  line-height: 1.7;
  padding: 6px 0 12px;
}

.proof-threads {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.proof-thread {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 10px 12px;
  background: var(--panel);
}

.proof-thread.is-resolved {
  opacity: 0.62;
}

.proof-thread.is-active {
  border-color: rgb(224 180 92 / 50%);
}

.proof-thread-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.proof-who {
  font-size: 12.5px;
  font-weight: 600;
}

.proof-chip {
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--line);
  font-size: 10.5px;
  color: var(--muted);
}

.proof-chip.is-ok {
  border-color: rgb(76 195 138 / 40%);
  color: #a5e6c7;
}

.proof-thread-body {
  margin: 6px 0 0;
  font-size: 13px;
  line-height: 1.6;
  overflow-wrap: anywhere;
}

.proof-reply {
  margin: 10px 0 0;
  padding-left: 10px;
  border-left: 2px solid var(--line);
}

.proof-thread-actions {
  display: flex;
  gap: 14px;
  margin-top: 8px;
}

.proof-link {
  padding: 0;
  font-size: 12px;
  color: var(--accent-strong);
}

.proof-reply-form,
.proof-composer {
  margin-top: 10px;
}

.proof-reply-form textarea,
.proof-composer textarea {
  width: 100%;
  resize: vertical;
  min-height: 54px;
  padding: 9px 11px;
  border-radius: 10px;
  border: 1px solid var(--line);
  background: var(--bg);
  color: var(--text);
  font: inherit;
  font-size: 13px;
}

.proof-reply-row,
.proof-composer-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-top: 8px;
}

.btn-small {
  padding: 7px 14px;
  font-size: 12.5px;
}

.proof-composer {
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--line);
}

.proof-draft {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  padding: 3px 10px;
  border-radius: 999px;
  border: 1px dashed rgb(224 180 92 / 55%);
  color: #f2d9a6;
  font-size: 11.5px;
}

.proof-readonly {
  margin: 4px 0 0;
  color: var(--muted);
  font-size: 12.5px;
}

.proof-error {
  margin: 8px 0 0;
  color: #ff9aa4;
  font-size: 12px;
}

.proof-rev-chip {
  padding: 1px 8px;
  border-radius: 999px;
  background: rgb(224 180 92 / 16%);
  border: 1px solid rgb(224 180 92 / 45%);
  color: var(--accent-strong);
  font-size: 10.5px;
  white-space: nowrap;
}

.muted {
  color: var(--muted);
}

.small {
  font-size: 11.5px;
}

@media (min-width: 900px) {
  .proof-lb-body {
    flex-direction: row;
  }

  .proof-panel {
    width: 380px;
    max-height: none;
    border-top: 0;
    border-left: 1px solid var(--line);
  }

  .proof-stage {
    padding: 14px 20px;
  }
}
</style>
