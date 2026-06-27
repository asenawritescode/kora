import { api } from './client'

export interface ExtensionRecord {
  name: string
  display_name: string
  description: string
  endpoint_url: string
  is_active: boolean
  subscriptions: string
  api_permissions: string
  secret_count: number
  consecutive_failures: number
  installed_at: string
  last_delivery_at: string
  last_error: string
}

export interface DeliveryRecord {
  id: string
  event_id: string
  event_type: string
  endpoint_url: string
  status: string
  attempt: number
  response_status: number
  duration_ms: number
  error_message: string
  created_at: string
}

export function fetchExtensions() {
  return api.get<ExtensionRecord[]>('/api/v1/system/extension')
}

export function createExtension(data: {
  name: string; display_name?: string; description?: string
  endpoint_url: string; subscriptions?: string; api_permissions?: string
}) {
  return api.post<{ name: string; secret: string; warning: string }>('/api/v1/system/extension', data)
}

export function deleteExtension(name: string) {
  return api.delete(`/api/v1/system/extension/${encodeURIComponent(name)}`)
}

export function fetchDeliveries(name: string) {
  return api.get<DeliveryRecord[]>(`/api/v1/system/extension/${encodeURIComponent(name)}/deliveries`)
}

export function rotateSecret(name: string) {
  return api.post<{ secret: string; warning: string }>(
    `/api/v1/system/extension/${encodeURIComponent(name)}/rotate-secret`
  )
}
