import { api } from './client'
import type { CurrentUser, LoginRequest, AuthProvider } from '@/types/api'

export async function login(req: LoginRequest): Promise<CurrentUser> {
  return api.post<CurrentUser>('/api/v1/auth/login', req)
}

export async function logout(): Promise<void> {
  return api.post<void>('/api/v1/auth/logout')
}

export async function fetchMe(): Promise<CurrentUser> {
  return api.get<CurrentUser>('/api/v1/auth/me')
}

export async function fetchProviders(): Promise<AuthProvider[]> {
  const data = await api.get<{ providers: AuthProvider[] }>('/api/v1/auth/providers')
  return data.providers
}
