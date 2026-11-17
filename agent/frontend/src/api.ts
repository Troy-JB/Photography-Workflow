import type {
  AlertEvent,
  CardInfo,
  DestinationInfo,
  Health,
  ImportDetail,
  ImportRow,
  PreviewResult,
  Project,
  SelftestReport,
  ServerState,
  Settings,
  Snapshot,
  StartRequest,
  StartResult,
  UpdateState,
} from './types'

function bridge(): Record<string, (...args: unknown[]) => Promise<unknown>> {
  const app = window.go?.binder?.App
  if (!app) {
    throw new Error('未检测到桌面运行时，请通过 PWF Agent 桌面程序打开。')
  }
  return app
}

export function isDesktop(): boolean {
  return Boolean(window.go?.binder?.App)
}

function normalizeError(err: unknown): string {
  if (!err) return '未知错误'
  if (typeof err === 'string') return err
  if (err instanceof Error) return err.message
  return String(err)
}

async function call<T>(method: string, ...args: unknown[]): Promise<T> {
  const fn = bridge()[method]
  if (typeof fn !== 'function') {
    throw new Error(`桌面接口不可用：${method}`)
  }
  try {
    return (await fn(...args)) as T
  } catch (err) {
    throw new Error(normalizeError(err))
  }
}

export const api = {
  health: () => call<Health>('Health'),
  version: () => call<string>('VersionString'),

  getSettings: () => call<Settings>('GetSettings'),
  saveSettings: (s: Settings) => call<void>('SaveSettings', s),
  defaultSettings: () => call<Settings>('DefaultSettings'),
  checkDestination: (path: string) => call<DestinationInfo>('CheckDestination', path),
  pickDirectory: (title: string, start: string) => call<string>('PickDirectory', title, start),

  cards: () => call<CardInfo[]>('Cards'),

  startImport: (req: StartRequest) => call<StartResult>('StartImport', req),
  runImport: (id: string) => call<void>('RunImport', id),
  pauseImport: () => call<void>('PauseImport'),
  snapshot: () => call<Snapshot>('Snapshot'),

  // Server connection
  server: () => call<ServerState>('Server'),
  saveServerConfig: (serverUrl: string, tenantSlug: string, bandwidthKbps: number) =>
    call<ServerState>('SaveServerConfig', serverUrl, tenantSlug, bandwidthKbps),
  login: (email: string, password: string) => call<ServerState>('Login', email, password),
  logout: () => call<void>('Logout'),
  projects: (limit = 100) => call<Project[]>('Projects', limit),
  attachProject: (importId: string, projectId: string) =>
    call<void>('AttachProject', importId, projectId),

  // Uploads
  startUpload: (importId: string) => call<void>('StartUpload', importId),
  pauseUpload: () => call<void>('PauseUpload'),

  // Updates
  updateStatus: () => call<UpdateState>('UpdateStatus'),
  checkUpdate: () => call<UpdateState>('CheckUpdate'),
  downloadUpdate: () => call<UpdateState>('DownloadUpdate'),
  installUpdate: () => call<UpdateState>('InstallUpdate'),

  imports: (limit = 100) => call<ImportRow[]>('Imports', limit),
  importDetail: (id: string, limit = 200) => call<ImportDetail>('ImportDetail', id, limit),
  previewFile: (fileId: string) => call<PreviewResult>('PreviewFile', fileId),

  alerts: () => call<AlertEvent[]>('Alerts'),
  ackAlert: (id: number) => call<boolean>('AckAlert', id),
  ackAllAlerts: () => call<number>('AckAllAlerts'),
  recentEvents: (limit = 200) => call<AlertEvent[]>('RecentEvents', limit),

  selftest: () => call<SelftestReport>('Selftest'),
}

/** Subscribes to pushed events; polling in App.vue stays the source of truth. */
export function onImportEvent(cb: () => void): () => void {
  const rt = window.runtime
  if (!rt?.EventsOn) return () => {}
  const names = [
    'import:event',
    'import:finished',
    'upload:event',
    'upload:finished',
    'server:changed',
    'update:changed',
    'update:progress',
  ]
  for (const name of names) rt.EventsOn(name, cb)
  return () => {
    for (const name of names) rt.EventsOff?.(name)
  }
}

/** Subscribes to the update event stream used by the settings panel. */
export function onUpdateEvent(cb: () => void): () => void {
  const rt = window.runtime
  if (!rt?.EventsOn) return () => {}
  const names = ['update:changed', 'update:progress']
  for (const name of names) rt.EventsOn(name, () => cb())
  return () => {
    for (const name of names) rt.EventsOff?.(name)
  }
}
