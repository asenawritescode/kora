import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

describe('theme persistence failures', () => {
  let classes: Set<string>
  let setItem: ReturnType<typeof vi.fn>

  beforeEach(() => {
    classes = new Set()
    setItem = vi.fn(() => { throw new Error('storage denied') })
    vi.stubGlobal('window', {
      localStorage: {
        getItem: vi.fn(() => { throw new Error('storage denied') }),
        setItem,
      },
      matchMedia: vi.fn(() => ({ matches: true })),
    })
    vi.stubGlobal('document', {
      documentElement: { classList: { toggle: (name: string, enabled: boolean) => enabled ? classes.add(name) : classes.delete(name) } },
    })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.resetModules()
  })

  it('loads with system theme and keeps session theme when storage is blocked', async () => {
    const { useUIStore } = await import('./ui-store')
    expect(useUIStore.getState().theme).toBe('dark')
    expect(() => useUIStore.getState().setTheme('light')).not.toThrow()
    expect(useUIStore.getState().theme).toBe('light')
    expect(classes.has('dark')).toBe(false)
    expect(setItem).toHaveBeenCalledWith('kora-theme', 'light')
  })
})
