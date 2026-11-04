import { api } from './client'
import type { CurrentUser } from '@/types/api'

export async function logout(): Promise<void> {
  return api.post<void>('/api/auth/logout')
}

export async function fetchMe(): Promise<CurrentUser> {
  return api.get<CurrentUser>('/api/auth/me')
}
