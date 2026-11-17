export function formatBytes(n: number): string {
  if (!n || n < 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = n
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
}

export function formatTime(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (v: number) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function today(): string {
  const d = new Date()
  const pad = (v: number) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

export function pauseLabel(reason?: string): string {
  switch (reason) {
    case 'card_removed':
      return '存储卡已拔出'
    case 'disk_full':
      return '磁盘空间不足'
    case 'destination_error':
      return '目标目录不可用'
    case 'user':
      return '用户暂停'
    case 'offline':
      return '网络离线'
    case 'auth_required':
      return '需要重新登录'
    case '':
    case undefined:
      return ''
    default:
      return reason
  }
}

export function statusLabel(status?: string): string {
  switch (status) {
    case 'open':
      return '拷贝中'
    case 'local_complete':
      return '本地完成'
    case 'uploading':
      return '上传中'
    case 'complete':
      return '全部完成'
    case 'failed':
      return '失败'
    case '':
    case undefined:
      return '未开始'
    default:
      return status
  }
}

export function statusKind(status?: string, paused?: string): 'ok' | 'warn' | 'err' | 'info' | 'idle' {
  if (paused) return 'warn'
  switch (status) {
    case 'complete':
    case 'local_complete':
      return 'ok'
    case 'open':
    case 'uploading':
      return 'info'
    case 'failed':
      return 'err'
    default:
      return 'idle'
  }
}

export function fileStateLabel(state: string): string {
  switch (state) {
    case 'discovered':
      return '待拷贝'
    case 'copying':
      return '拷贝中'
    case 'copied':
      return '已写入'
    case 'local_verified':
      return '本地校验通过'
    case 'registered':
      return '已登记'
    case 'uploading':
      return '上传中'
    case 'uploaded':
      return '已上传'
    case 'remote_verified':
      return '云端校验通过'
    case 'complete':
      return '完成'
    case 'copy_failed':
      return '拷贝失败'
    case 'hash_mismatch':
      return '哈希不一致'
    case 'upload_failed':
      return '上传失败'
    case 'remote_corrupt':
      return '云端损坏'
    default:
      return state
  }
}
