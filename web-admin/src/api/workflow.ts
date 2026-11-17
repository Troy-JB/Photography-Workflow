import { api } from '../state/session'

export type WorkflowStage =
  | 'not_shot'
  | 'shooting'
  | 'originals_delivery'
  | 'selecting'
  | 'retouch_delivery'
  | 'completed'
  | 'cancelled'

export interface WorkflowItem {
  id: string
  code: string
  title: string
  client_id?: string
  client_name: string
  project_state: string
  workflow_stage: WorkflowStage
  workflow_stage_label: string
  payment_status: string
  currency: string
  amount_cents: number
  shoot_at?: string
  order_at: string
  created_at: string
  selection_due_at?: string
  proof_due_at?: string
  cover_url?: string
  gallery_id?: string
  proof_round_id?: string
  delivery_id?: string
  delivery_status?: string
  gallery_submitted: boolean
}

export interface WorkflowNotification {
  id: string
  type: string
  title: string
  body: string
  status: string
  due_at: string
  created_at: string
  read_at?: string
  subject_type: string
  subject_id: string
  payload: Record<string, unknown>
}

export const WORKFLOW_STAGES: { id: WorkflowStage; label: string }[] = [
  { id: 'not_shot', label: '未拍摄' },
  { id: 'shooting', label: '拍摄中' },
  { id: 'originals_delivery', label: '原片交付中' },
  { id: 'selecting', label: '选片中' },
  { id: 'retouch_delivery', label: '精修交付中' },
  { id: 'completed', label: '已完成' },
]

export function listWorkflow(stage?: WorkflowStage): Promise<{ items: WorkflowItem[]; total: number }> {
  const query = stage ? `?stage=${encodeURIComponent(stage)}` : ''
  return api(`/api/v1/workflow${query}`)
}

export function getWorkbench(): Promise<{ items: WorkflowItem[]; total: number }> {
  return api('/api/v1/workbench')
}

export function listWorkflowNotifications(): Promise<{ items: WorkflowNotification[]; total: number }> {
  return api('/api/v1/notifications?unread=true&limit=200')
}

export function clientGroupKey(item: WorkflowItem): string {
  return item.client_id || item.id
}

export function isOnDate(value: string | undefined, date: Date): boolean {
  if (!value) return false
  const d = new Date(value)
  return d.getFullYear() === date.getFullYear() && d.getMonth() === date.getMonth() && d.getDate() === date.getDate()
}

export function orderFrequency(items: WorkflowItem[], days: 7 | 30 | 90, now = new Date()): { date: Date; count: number }[] {
  const result = Array.from({ length: days }, (_, index) => {
    const date = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (days - index - 1))
    return { date, count: 0 }
  })
  const byDay = new Map(result.map((item) => [dateKey(item.date), item]))
  for (const item of items) {
    const day = byDay.get(dateKey(new Date(item.order_at)))
    if (day) day.count++
  }
  return result
}

function dateKey(date: Date): string {
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`
}
