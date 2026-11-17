// Mirrors the Go JSON contracts in internal/model, internal/core and
// internal/binder. The Wails bridge serializes with encoding/json, so these
// field names are the wire format.

export type ImportStatus = 'open' | 'local_complete' | 'uploading' | 'complete' | 'failed'

export type FileState =
  | 'discovered'
  | 'copying'
  | 'copied'
  | 'local_verified'
  | 'registered'
  | 'uploading'
  | 'uploaded'
  | 'remote_verified'
  | 'complete'
  | 'copy_failed'
  | 'hash_mismatch'
  | 'upload_failed'
  | 'remote_corrupt'

export interface Health {
  version: string
  db_path: string
  settings_path: string
  ok: boolean
  error?: string
}

export interface Settings {
  dest_a: string
  dest_b: string
  require_backup: boolean
  workers: number
  project_code: string
  exiftool_path?: string
  ffmpeg_path?: string
  server_url?: string
  tenant_slug?: string
  bandwidth_kbps?: number
}

export interface Project {
  id: string
  code: string
  title: string
  state: string
}

export interface ServerInfo {
  server_url: string
  logged_in: boolean
  email?: string
  user_name?: string
  tenant_name?: string
  tenant_slug?: string
  role?: string
  device_id?: string
  device_name?: string
  fingerprint?: string
}

export interface ServerState {
  configured: boolean
  info: ServerInfo
  error?: string
  device_error?: string
}

export interface UploadResult {
  import_id: string
  batch_id?: string
  paused: boolean
  reason?: string
  message?: string
  total: number
  uploaded: number
  remote_verified: number
  failed: number
  pending: number
}

export interface UploadState {
  import_id?: string
  running: boolean
  result?: UploadResult
  error?: string
}

export interface UpdateState {
  configured: boolean
  supported: boolean
  channel?: string
  current: string
  latest?: string
  available: boolean
  mandatory: boolean
  notes?: string
  checking: boolean
  downloaded: boolean
  downloaded_size?: number
  downloading: boolean
  progress: number
  installing: boolean
  /** Dev-channel installers are not Authenticode-signed; the UI says so. */
  unsigned: boolean
  checked_at?: string
  error?: string
  message?: string
}

export interface CardInfo {
  root: string
  guid: string
  label: string
  serial: number
  capacity: number
  fingerprint: string
}

export interface DestinationInfo {
  path: string
  exists: boolean
  writable: boolean
  free_bytes: number
  error?: string
}

export interface FileCounts {
  total: number
  local_verified: number
  uploaded: number
  remote_verified: number
  failed: number
}

export interface ImportRecord {
  id: string
  project_id: string
  project_code: string
  server_batch_id: string
  card_id: string
  source_root: string
  shoot_date: string
  dest_a: string
  dest_b: string
  require_backup: boolean
  status: ImportStatus
  pause_reason: string
  created_at: string
  completed_at?: string
}

export interface AlertEvent {
  id: number
  level: 'info' | 'warning' | 'error'
  code: string
  import_id?: string
  file_id?: string
  message: string
  acknowledged: boolean
  created_at: string
}

export interface RunResult {
  import_id: string
  paused: boolean
  reason?: string
  message?: string
  total: number
  local_verified: number
  failed: number
}

export interface Snapshot {
  running: boolean
  import?: ImportRecord
  counts: FileCounts
  alerts: AlertEvent[]
  unacked_total: number
  last_result?: RunResult
  last_error?: string
  backup_disabled: boolean
  server: ServerState
  upload: UploadState
  update: UpdateState
}

export interface StartRequest {
  source: string
  project_code: string
  project_id: string
  shoot_date: string
  dest_a: string
  dest_b: string
  require_backup: boolean
  allow_duplicate: boolean
}

export interface StartResult {
  import: ImportRecord
  added: number
  existing: boolean
}

export interface CopyRecord {
  file_id: string
  dest: 'A' | 'B'
  final_path?: string
  temp_path?: string
  state: string
  bytes: number
  sha256?: string
  error?: string
}

export interface FileRecord {
  id: string
  import_id: string
  rel_path: string
  size: number
  mtime_ns: number
  kind: string
  role: string
  pair_key?: string
  sha256?: string
  mime?: string
  width?: number
  height?: number
  captured_at?: string
  camera?: string
  lens?: string
  state: FileState
  server_asset_id?: string
  error?: string
}

export interface FileRow {
  file: FileRecord
  copies: CopyRecord[]
}

export interface PreviewResult {
  file_id: string
  data_url: string
  size: number
}

export interface ImportDetail {
  status: {
    import: ImportRecord
    counts: FileCounts
    unacknowledged_events: AlertEvent[]
  }
  files: FileRow[]
  total_files: number
}

export interface ImportRow {
  import: ImportRecord
  counts: FileCounts
  card_label: string
}

export interface SelftestStep {
  name: string
  ok: boolean
  detail?: string
  duration_ms: number
}

export interface SelftestReport {
  ok: boolean
  steps: SelftestStep[]
}
