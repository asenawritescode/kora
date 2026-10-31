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
  try {
    return localStorage.getItem(TOKEN_KEY)
  } catch {
    return null
  }
}

function loadEmail(): string | null {
  try {
    return localStorage.getItem(EMAIL_KEY)
  } catch {
    return null
  }
}

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback
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
      try {
        localStorage.setItem(TOKEN_KEY, result.token)
        localStorage.setItem(EMAIL_KEY, result.email)
      } catch {
        try {
          localStorage.removeItem(TOKEN_KEY)
          localStorage.removeItem(EMAIL_KEY)
        } catch {
          // Storage is unavailable; the unauthenticated in-memory state remains authoritative.
        }
        throw new Error('Console login succeeded, but browser storage is unavailable so the session could not be saved.')
      }
      set({
        token: result.token,
        email: result.email,
        needsPasswordChange: result.needs_password_change,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (err: unknown) {
      set({ isLoading: false, error: errorMessage(err, 'Login failed') })
      throw err
    }
  },

  logout: () => {
    try {
      localStorage.removeItem(TOKEN_KEY)
      localStorage.removeItem(EMAIL_KEY)
    } catch {
      // Clear in-memory credentials even when the browser blocks persistence.
    }
    set({ token: null, email: null, isAuthenticated: false, needsPasswordChange: false })
  },

  changePassword: async (newPassword) => {
    set({ isLoading: true, error: null })
    try {
      await consoleApi.changePassword(newPassword)
      set({ needsPasswordChange: false, isLoading: false })
    } catch (err: unknown) {
      set({ isLoading: false, error: errorMessage(err, 'Failed') })
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
      try {
        localStorage.removeItem(TOKEN_KEY)
        localStorage.removeItem(EMAIL_KEY)
      } catch {
        // Clear in-memory credentials even when the browser blocks persistence.
      }
      set({ token: null, email: null, isAuthenticated: false, isLoading: false })
      return false
    }
  },

  clearError: () => set({ error: null }),
}))
