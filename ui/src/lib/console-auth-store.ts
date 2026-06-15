import { create } from 'zustand'
import * as consoleApi from './api/console'

interface ConsoleAuthState {
  token: string | null
  email: string | null
  needsPasswordChange: boolean
  isAuthenticated: boolean
  isLoading: boolean
  error: string | null

  login: (email: string, password: string) => Promise<void>
  logout: () => void
  changePassword: (newPassword: string) => Promise<void>
  checkAuth: () => Promise<boolean>
  clearError: () => void
}

const TOKEN_KEY = 'kora_console_token'
const EMAIL_KEY = 'kora_console_email'

function loadToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

function loadEmail(): string | null {
  return localStorage.getItem(EMAIL_KEY)
}

export const useConsoleAuthStore = create<ConsoleAuthState>((set, get) => ({
  token: loadToken(),
  email: loadEmail(),
  needsPasswordChange: false,
  isAuthenticated: !!loadToken(),
  isLoading: !loadToken(), // Only show loading if we have a token to validate
  error: null,

  login: async (email, password) => {
    set({ isLoading: true, error: null })
    try {
      const result = await consoleApi.login(email, password)
      localStorage.setItem(TOKEN_KEY, result.token)
      localStorage.setItem(EMAIL_KEY, result.email)
      set({
        token: result.token,
        email: result.email,
        needsPasswordChange: result.needs_password_change,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (err: any) {
      set({ isLoading: false, error: err.message || 'Login failed' })
      throw err
    }
  },

  logout: () => {
    localStorage.removeItem(TOKEN_KEY)
    localStorage.removeItem(EMAIL_KEY)
    set({ token: null, email: null, isAuthenticated: false, needsPasswordChange: false })
  },

  changePassword: async (newPassword) => {
    set({ isLoading: true, error: null })
    try {
      await consoleApi.changePassword(newPassword)
      set({ needsPasswordChange: false, isLoading: false })
    } catch (err: any) {
      set({ isLoading: false, error: err.message || 'Failed' })
      throw err
    }
  },

  checkAuth: async () => {
    const token = get().token
    if (!token) {
      set({ isAuthenticated: false, isLoading: false })
      return false
    }
    // Validate by listing sites — if it fails with 401, token is invalid.
    set({ isLoading: true })
    try {
      await consoleApi.listSites()
      set({ isAuthenticated: true, isLoading: false })
      return true
    } catch {
      localStorage.removeItem(TOKEN_KEY)
      localStorage.removeItem(EMAIL_KEY)
      set({ token: null, email: null, isAuthenticated: false, isLoading: false })
      return false
    }
  },

  clearError: () => set({ error: null }),
}))
