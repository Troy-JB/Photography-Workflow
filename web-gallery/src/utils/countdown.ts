export interface Countdown {
  expired: boolean
  days: number
  hours: number
  minutes: number
  seconds: number
  text: string
}

export function countdown(expiresAt: number, now: number): Countdown {
  const remaining = expiresAt - now
  if (!Number.isFinite(expiresAt) || remaining <= 0) {
    return { expired: true, days: 0, hours: 0, minutes: 0, seconds: 0, text: '已截止' }
  }
  const total = Math.floor(remaining / 1000)
  const days = Math.floor(total / 86_400)
  const hours = Math.floor((total % 86_400) / 3_600)
  const minutes = Math.floor((total % 3_600) / 60)
  const seconds = total % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  const clock = `${pad(hours)}:${pad(minutes)}:${pad(seconds)}`
  return {
    expired: false,
    days,
    hours,
    minutes,
    seconds,
    text: days > 0 ? `${days} 天 ${clock}` : clock,
  }
}
