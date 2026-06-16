import type { ConsoleSite } from '@/types/api'

const BASE = '/api/console'

function getToken(): string | null {
  return localStorage.getItem('kora_console_token')
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

export async function createSite(data: {
  hostname: string; db_type: string; db_host: string; db_port: number; db_name: string
  db_user: string; db_password: string; domains?: string
  admin_email: string; admin_password: string; admin_full_name?: string
}): Promise<{ hostname: string; db_name: string; status: string; admin: string }> {
  const resp = await fetch(BASE + '/sites', {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify(data),
    credentials: 'same-origin',
  })
  const json = await resp.json()
  if (!resp.ok) {
    const msg = typeof json.error === 'string'
      ? json.error
      : json.error?.message || json.error?.errors?.[0]?.message || 'Failed to create site'
    throw new Error(msg)
  }
  return json.data
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
