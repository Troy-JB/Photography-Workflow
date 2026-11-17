import { afterEach, describe, expect, it } from 'vitest'
import type { RouteLocationNormalized } from 'vue-router'
import { resolveAuthNavigation } from './auth-navigation'
import { auth } from './state/session'

function location(name: string, fullPath: string): Pick<RouteLocationNormalized, 'name' | 'fullPath'> {
  return { name, fullPath } as Pick<RouteLocationNormalized, 'name' | 'fullPath'>
}

afterEach(() => {
  auth.status = 'anonymous'
  auth.role = ''
})

describe('resolveAuthNavigation', () => {
  it('requires a session for admin routes and remembers the destination', () => {
    auth.status = 'anonymous'

    expect(resolveAuthNavigation(location('galleries', '/galleries'))).toEqual({ name: 'login', query: { next: '/galleries' } })
  })

  it('allows auth pages for anonymous users', () => {
    auth.status = 'anonymous'

    expect(resolveAuthNavigation(location('login', '/login'))).toBe(true)
  })

  it('keeps the official website public for anonymous visitors', () => {
    auth.status = 'anonymous'

    expect(resolveAuthNavigation(location('home', '/'))).toBe(true)
  })

  it('sends authenticated users away from login and registration', () => {
    auth.status = 'ready'

    expect(resolveAuthNavigation(location('register', '/register'))).toEqual({ name: 'workbench' })
  })

  it('requires a photographer or staff session for schedule management', () => {
    auth.status = 'anonymous'
    expect(resolveAuthNavigation(location('schedule', '/schedule'))).toEqual({ name: 'login', query: { next: '/schedule' } })

    auth.status = 'ready'
    auth.role = 'assistant'
    expect(resolveAuthNavigation(location('schedule', '/schedule'))).toEqual({ name: 'workbench' })

    auth.role = 'owner'
    expect(resolveAuthNavigation(location('schedule', '/schedule'))).toBe(true)
  })
})
