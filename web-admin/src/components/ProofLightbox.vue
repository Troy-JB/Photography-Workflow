<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { Annotation, ProofComment, ProofRoundItem } from '../api/proof'
import { listProofComments, resolveProofComment } from '../api/proof'
import { annotationLabel, formatDateTime } from '../utils/format'
import { toast } from '../state/toast'
import AnnotationOverlay from './AnnotationOverlay.vue'

const props = defineProps<{
  roundId: string
  item: ProofRoundItem
  canResolve: boolean
}>()
const emit = defineEmits<{ close: []; changed: [] }>()

const comments = ref<ProofComment[]>([])
const includeResolved = ref(false)
const loading = ref(false)
const error = ref('')
const busyId = ref('')
const activeId = ref('')
const naturalWidth = ref(props.item.width)
const naturalHeight = ref(props.item.height)

const annotations = computed<{ id: string; annotation: Annotation }[]>(() =>
  comments.value
    .filter((c) => c.annotation && (!c.resolved_at || includeResolved.value))
    .map((c) => ({ id: c.id, annotation: c.annotation as Annotation })),
)

const threads = computed(() => {
  const roots = comments.value.filter((c) => !c.parent_id)
  const replies = new Map<string, ProofComment[]>()
  for (const comment of comments.value) {
    if (!comment.parent_id) continue
    const list = replies.get(comment.parent_id) ?? []
    list.push(comment)
    replies.set(comment.parent_id, list)
  }
  return roots.map((root) => ({ root, replies: replies.get(root.id) ?? [] }))
})

const openCount = computed(() => comments.value.filter((c) => !c.resolved_at).length)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    comments.value = await listProofComments(props.roundId, {
      assetId: props.item.asset_id,
      includeResolved: includeResolved.value,
    })
    const first = annotations.value[0]
    activeId.value = first ? first.id : ''
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载批注失败'
  } finally {
    loading.value = false
  }
}

async function resolve(comment: ProofComment): Promise<void> {
  if (!props.canResolve || comment.resolved_at || busyId.value) return
  busyId.value = comment.id
  try {
    await resolveProofComment(props.roundId, comment.id)
    toast('批注已标记为已解决', 'success')
    await load()
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '操作失败', 'error')
  } finally {
    busyId.value = ''
  }
}

function focusAnnotation(comment: ProofComment): void {
  if (comment.annotation) activeId.value = comment.id
}

function onImageLoad(event: Event): void {
  const image = event.target as HTMLImageElement
  if (image.naturalWidth > 0) {
    naturalWidth.value = image.naturalWidth
    naturalHeight.value = image.naturalHeight
  }
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') emit('close')
}

watch(includeResolved, () => void load())
watch(
  () => props.item.asset_id,
  () => void load(),
)

onMounted(() => {
  void load()
  document.addEventListener('keydown', onKeydown)
  document.body.classList.add('modal-open')
})

onUnmounted(() => {
  document.removeEventListener('keydown', onKeydown)
  document.body.classList.remove('modal-open')
})
</script>

<template>
  <Teleport to="body">
    <div class="lightbox" role="dialog" aria-modal="true" :aria-label="`${item.filename} 批注`" @click.self="emit('close')">
      <div class="lightbox-panel">
        <header class="lightbox-head">
          <div>
            <strong>{{ item.filename }}</strong>
            <span class="muted small">
              #{{ item.position }} · {{ item.width }}×{{ item.height }}
              <template v-if="item.revision_no > 0"> · 修图版 v{{ item.revision_no }}</template>
            </span>
          </div>
          <div class="lightbox-head-actions">
            <label class="checkbox-inline">
              <input v-model="includeResolved" type="checkbox" />
              显示已解决
            </label>
            <span class="pill" :class="openCount > 0 ? 'warn' : 'ok'">未解决 {{ openCount }}</span>
            <button class="btn ghost sm" type="button" @click="emit('close')">关闭</button>
          </div>
        </header>

        <div class="lightbox-body">
          <div class="lightbox-stage">
            <div class="lightbox-frame">
              <img
                v-if="item.display_url"
                :src="item.display_url"
                :alt="item.filename"
                @load="onImageLoad"
              />
              <p v-else class="empty muted">这张素材还没有可用的预览图。</p>
              <AnnotationOverlay
                v-if="annotations.length > 0"
                :annotations="annotations"
                :width="naturalWidth"
                :height="naturalHeight"
                :active-id="activeId"
              />
            </div>
          </div>

          <aside class="lightbox-thread">
            <p v-if="error" class="alert error" role="alert">{{ error }}</p>
            <p v-else-if="loading" class="empty muted">加载批注…</p>
            <p v-else-if="threads.length === 0" class="empty muted">这张素材还没有批注。</p>

            <article v-for="thread in threads" :key="thread.root.id" class="thread">
              <div
                class="comment-card"
                :class="{ active: activeId === thread.root.id, resolved: thread.root.resolved_at }"
                @mouseenter="focusAnnotation(thread.root)"
                @click="focusAnnotation(thread.root)"
              >
                <header>
                  <span class="pill" :class="thread.root.author_type === 'client' ? 'warn' : 'ok'">
                    {{ thread.root.author_type === 'client' ? '客户' : '员工' }}
                  </span>
                  <span v-if="thread.root.annotation" class="muted small">
                    {{ annotationLabel(thread.root.annotation.type) }}
                  </span>
                  <span class="spacer"></span>
                  <span class="muted small">{{ formatDateTime(thread.root.created_at) }}</span>
                </header>
                <p v-if="thread.root.body" class="comment-body">{{ thread.root.body }}</p>
                <footer>
                  <span v-if="thread.root.resolved_at" class="muted small">
                    已解决 · {{ formatDateTime(thread.root.resolved_at) }}
                  </span>
                  <button
                    v-else-if="canResolve"
                    class="btn ghost sm"
                    type="button"
                    :disabled="busyId === thread.root.id"
                    @click.stop="resolve(thread.root)"
                  >
                    {{ busyId === thread.root.id ? '处理中…' : '标记已解决' }}
                  </button>
                </footer>
              </div>

              <div v-for="reply in thread.replies" :key="reply.id" class="comment-reply">
                <header>
                  <span class="pill" :class="reply.author_type === 'client' ? 'warn' : 'ok'">
                    {{ reply.author_type === 'client' ? '客户' : '员工' }}
                  </span>
                  <span class="spacer"></span>
                  <span class="muted small">{{ formatDateTime(reply.created_at) }}</span>
                </header>
                <p v-if="reply.body" class="comment-body">{{ reply.body }}</p>
                <footer>
                  <span v-if="thread.root.resolved_at" class="muted small">已解决</span>
                  <span v-else class="muted small">随主批注一并解决</span>
                </footer>
              </div>
            </article>
          </aside>
        </div>
      </div>
    </div>
  </Teleport>
</template>
