import { create } from 'zustand'
import type { CurrentUser } from '@/types/api'
import { sitePath } from './basepath'
import * as authApi from './api/auth'

interface AuthState {
  user: CurrentUser | null
  isAuthenticated: boolean
  isLoading: boolean
  logout: () => Promise<void>
  checkAuth: () => Promise<boolean>
}

export const useAuthStore = create<AuthState>((set, get) => ({
  user: null,
  isAuthenticated: false,
  // AuthGuard checks protected workspace routes on entry.
  isLoading: false,

  logout: async () => {
    try {
      await authApi.logout()
    } catch {
      // Clear local state even when the tenant session has already expired.
    }
    set({ user: null, isAuthenticated: false, isLoading: false })
    window.location.href = sitePath('/workspace/auth/login')
  },

  checkAuth: async () => {
    if (get().isAuthenticated) return true
    set({ isLoading: true })
    try {
      const user = await authApi.fetchMe()
      set({ user, isAuthenticated: true, isLoading: false })
      return true
    } catch {
      set({ user: null, isAuthenticated: false, isLoading: false })
      return false
    }
  },
}))
