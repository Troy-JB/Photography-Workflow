import { apiRequest } from './client'
import type { Album, ManifestPage, SelectionBody, SelectionResult, SessionView } from './types'

export function createSession(shareToken: string): Promise<SessionView> {
  return apiRequest<SessionView>('/g/v1/session', { method: 'POST', body: { token: shareToken } })
}

export function fetchAlbum(session: string): Promise<Album> {
  return apiRequest<Album>('/g/v1/session', { session })
}

export function fetchManifest(
  session: string,
  albumID: string,
  cursor: string,
  limit: number,
): Promise<ManifestPage> {
  const query = new URLSearchParams({ limit: String(limit) })
  if (cursor) query.set('cursor', cursor)
  return apiRequest<ManifestPage>(`/g/v1/albums/${encodeURIComponent(albumID)}/manifest?${query}`, {
    session,
  })
}

export function applySelections(
  session: string,
  body: SelectionBody,
  idempotencyKey: string,
): Promise<SelectionResult> {
  return apiRequest<SelectionResult>('/g/v1/selections', {
    method: 'POST',
    session,
    body,
    idempotencyKey,
  })
}

export function submitAlbum(
  session: string,
  albumID: string,
  expectedVersion: number,
  idempotencyKey: string,
): Promise<Album> {
  return apiRequest<Album>(`/g/v1/albums/${encodeURIComponent(albumID)}/submit`, {
    method: 'POST',
    session,
    body: { expected_version: expectedVersion },
    idempotencyKey,
  })
}
