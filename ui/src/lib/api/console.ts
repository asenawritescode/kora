import type { ConsoleSite } from '@/types/api'

const BASE = '/api/console'

function getToken(): string | null {
  try {
    return localStorage.getItem('kora_console_token')
  } catch {
    return null
  }
}

function headers(): Record<string, string> {
  const h: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = getToken()
  if (token) h['Authorization'] = `Bearer ${token}`
  return h
}

export async function login(email: string, password: string): Promise<{ token: string; email: string; needs_password_change: boolean }> {
  const resp = await fetch(BASE + '/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
    credentials: 'same-origin',
  })
  const json = await resp.json()
  if (!resp.ok) throw new Error(json.error?.message || json.error || 'Login failed')
  return json.data
}

export async function changePassword(newPassword: string): Promise<void> {
  const resp = await fetch(BASE + '/change-password', {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify({ new_password: newPassword }),
    credentials: 'same-origin',
  })
  if (!resp.ok) {
    const json = await resp.json()
    throw new Error(json.error?.message || 'Failed')
  }
}

export async function listSites(): Promise<ConsoleSite[]> {
  const resp = await fetch(BASE + '/sites', { headers: headers(), credentials: 'same-origin' })
  const json = await resp.json()
  if (!resp.ok) throw new Error(json.error?.message || 'Failed to list sites')
  return json.data || []
}

export async function updateSite(name: string, domains: string[]): Promise<void> {
  const resp = await fetch(BASE + '/sites/' + encodeURIComponent(name), {
    method: 'PUT',
    headers: headers(),
    body: JSON.stringify({ domains }),
    credentials: 'same-origin',
  })
  if (!resp.ok) {
    const json = await resp.json()
    throw new Error(json.error?.message || 'Failed to update site')
  }
}

export async function updateSiteStorage(
  name: string,
  data: { domains?: string[]; file_storage?: string; storage_bucket?: string },
): Promise<void> {
  const resp = await fetch(BASE + '/sites/' + encodeURIComponent(name), {
    method: 'PUT',
    headers: headers(),
    body: JSON.stringify(data),
    credentials: 'same-origin',
  })
  if (!resp.ok) {
    const json = await resp.json()
    throw new Error(json.error?.message || 'Failed to update site')
  }
}

export async function deleteSite(name: string): Promise<void> {
  const resp = await fetch(BASE + '/sites/' + encodeURIComponent(name), {
    method: 'DELETE',
    headers: headers(),
    body: JSON.stringify({ confirm: name }),
    credentials: 'same-origin',
  })
  if (!resp.ok) {
    const json = await resp.json()
    throw new Error(json.error?.message || 'Failed to delete site')
  }
}
