export function formatDateTime(value?: string | null): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function formatDate(value?: string | null): string {
  if (!value) return '—'
  return formatDateTime(value).slice(0, 10)
}

// datetime-local inputs speak local time without a zone; convert both ways so
// deadlines are not silently shifted by the browser's timezone.
export function toLocalInput(value?: string | null): string {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function fromLocalInput(value: string): string | null {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date.toISOString()
}

export function deadlineLabel(value?: string | null, now = Date.now()): string {
  if (!value) return '不限时'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '不限时'
  const delta = date.getTime() - now
  if (delta <= 0) return `已过期 · ${formatDateTime(value)}`
  const hours = Math.floor(delta / 3_600_000)
  if (hours < 24) return `剩余 ${Math.max(1, hours)} 小时 · ${formatDateTime(value)}`
  return `剩余 ${Math.floor(hours / 24)} 天 · ${formatDateTime(value)}`
}

const STATUS_LABELS: Record<string, string> = {
  draft: '草稿',
  published: '已发布',
  expired: '已过期',
  revoked: '已撤销',
}

export function statusLabel(status: string): string {
  return STATUS_LABELS[status] ?? status
}

const DECISION_LABELS: Record<string, string> = {
  selected: '已选',
  rejected: '已弃',
  none: '未决定',
}

export function decisionLabel(decision: string): string {
  return DECISION_LABELS[decision] ?? decision
}

const PROOF_STATUS_LABELS: Record<string, string> = {
  open: '待客户查看',
  in_review: '客户审阅中',
  changes_requested: '客户提出修改',
  retouching: '修图中',
  resubmitted: '已回传待确认',
  approved: '已通过',
}

export function proofStatusLabel(status: string): string {
  return PROOF_STATUS_LABELS[status] ?? status
}

const PROOF_STATUS_TONES: Record<string, string> = {
  open: 'draft',
  in_review: 'warn',
  changes_requested: 'warn',
  retouching: 'warn',
  resubmitted: 'published',
  approved: 'ok',
}

export function proofStatusTone(status: string): string {
  return PROOF_STATUS_TONES[status] ?? 'muted'
}

const ANNOTATION_LABELS: Record<string, string> = {
  point: '标记点',
  rect: '矩形框选',
  ellipse: '椭圆圈选',
  freehand: '手绘',
}

export function annotationLabel(type?: string): string {
  return type ? (ANNOTATION_LABELS[type] ?? type) : ''
}

export function bytes(size: number): string {
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(0)} KB`
  if (size < 1024 * 1024 * 1024) return `${(size / 1024 / 1024).toFixed(1)} MB`
  return `${(size / 1024 / 1024 / 1024).toFixed(2)} GB`
}
