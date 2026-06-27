import { api } from './client'
import type { CurrentUser, LoginRequest, AuthProvider } from '@/types/api'

export async function login(req: LoginRequest): Promise<CurrentUser> {
  return api.post<CurrentUser>('/api/auth/login', req)
}

export async function logout(): Promise<void> {
  return api.post<void>('/api/auth/logout')
}

export async function fetchMe(): Promise<CurrentUser> {
  return api.get<CurrentUser>('/api/auth/me')
}

export async function fetchProviders(): Promise<AuthProvider[]> {
  const data = await api.get<{ providers: AuthProvider[] }>('/api/auth/providers')
  return data.providers
}
