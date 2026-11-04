import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('./api/console', () => ({
  login: vi.fn(),
  listSites: vi.fn(),
  changePassword: vi.fn(),
}))

afterEach(() => {
  vi.unstubAllGlobals()
  vi.resetModules()
  vi.clearAllMocks()
})

describe('console auth storage', () => {
  it('starts unauthenticated when storage reads are blocked', async () => {
    vi.stubGlobal('localStorage', {
      getItem: () => { throw new DOMException('storage disabled', 'SecurityError') },
      setItem: () => undefined,
      removeItem: () => undefined,
    })
    const { useConsoleAuthStore } = await import('./console-auth-store')

    expect(useConsoleAuthStore.getState().token).toBeNull()
    expect(useConsoleAuthStore.getState().isAuthenticated).toBe(false)
  })

  it('reports when a server login cannot be persisted locally', async () => {
    vi.stubGlobal('localStorage', {
      getItem: () => null,
      setItem: () => { throw new DOMException('quota exceeded', 'QuotaExceededError') },
      removeItem: () => { throw new DOMException('storage disabled', 'SecurityError') },
    })
    const consoleApi = await import('./api/console')
    vi.mocked(consoleApi.login).mockResolvedValue({ token: 'token', email: 'user@example.test', needs_password_change: false })
    const { useConsoleAuthStore } = await import('./console-auth-store')

    await expect(useConsoleAuthStore.getState().login('user@example.test', 'password'))
      .rejects.toThrow('browser storage is unavailable')
    expect(useConsoleAuthStore.getState().isAuthenticated).toBe(false)
    expect(useConsoleAuthStore.getState().error).toContain('session could not be saved')
  })
})
