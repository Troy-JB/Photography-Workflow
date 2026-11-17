import type { RouteLocationNormalized, RouteLocationRaw } from 'vue-router'
import { auth } from './state/session'

export function resolveAuthNavigation(
  to: Pick<RouteLocationNormalized, 'name' | 'fullPath'>,
): true | RouteLocationRaw {
  const isPublic = to.name === 'home' || to.name === 'login' || to.name === 'register'
  const isAuthPage = to.name === 'login' || to.name === 'register'
  if (isAuthPage && auth.status === 'ready') {
    return { name: 'workbench' }
  }
  if (!isPublic && auth.status !== 'ready') {
    return { name: 'login', query: { next: to.fullPath } }
  }
  if (to.fullPath.startsWith('/schedule')) {
    if (auth.role !== 'owner' && auth.role !== 'staff') return { name: 'workbench' }
  }
  return true
}
