import { api } from './client'
import type { PageManifest } from '@/manifest/schema/page'

export interface PageManifestListEntry {
  name: string
  route: string
  layout: string
  label: string
  module: string
  status: string
}

export async function fetchPageManifests(): Promise<PageManifestListEntry[]> {
  return api.get<PageManifestListEntry[]>('/api/v1/system/page-manifests')
}

export async function fetchPageManifestByName(name: string): Promise<PageManifest> {
  return api.get<PageManifest>(`/api/v1/system/page-manifests/${encodeURIComponent(name)}`)
}

export async function fetchPageManifestByRoute(route: string, version?: string): Promise<PageManifest> {
  const params = new URLSearchParams({ route })
  if (version) params.set('version', version)
  return api.get<PageManifest>(`/api/v1/page-manifests?${params.toString()}`)
}

export async function createPageManifest(data: PageManifest): Promise<unknown> {
  return api.post('/api/v1/system/page-manifests', data)
}

export async function updatePageManifest(name: string, data: PageManifest): Promise<unknown> {
  return api.put(`/api/v1/system/page-manifests/${encodeURIComponent(name)}`, data)
}

export async function deletePageManifest(name: string): Promise<unknown> {
  return api.delete(`/api/v1/system/page-manifests/${encodeURIComponent(name)}`)
}

export async function executeViewAction(
  actionId: string,
  payload: { view: string; component: string; context: Record<string, unknown> },
): Promise<unknown> {
  return api.post(`/api/v1/view/action/${encodeURIComponent(actionId)}`, payload)
}
