import { create } from 'zustand'

type Theme = 'light' | 'dark'
type ShellMode = 'simple' | 'advanced'

interface UIState {
  theme: Theme
  sidebarOpen: boolean
  sidebarCollapsed: boolean
  shellMode: ShellMode
  activeModule: string | null
  configVersion: number

  toggleTheme: () => void
  setTheme: (theme: Theme) => void
  toggleSidebar: () => void
  setSidebarOpen: (open: boolean) => void
  setSidebarCollapsed: (collapsed: boolean) => void
  setShellMode: (mode: ShellMode) => void
  setActiveModule: (module: string | null) => void
  setConfigVersion: (version: number) => void
}

function getStoredTheme(): Theme {
  if (typeof window === 'undefined') return 'light'
  let stored: string | null = null
  try {
    stored = window.localStorage.getItem('kora-theme')
  } catch {
    // Theme is a preference; blocked storage must not prevent the app loading.
  }
  if (stored === 'dark' || stored === 'light') return stored
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  } catch {
    return 'light'
  }
}

function applyTheme(theme: Theme) {
  if (typeof document === 'undefined') return
  document.documentElement.classList.toggle('dark', theme === 'dark')
  try {
    window.localStorage.setItem('kora-theme', theme)
  } catch {
    // Keep the current session's theme even when persistence is unavailable.
  }
}

export const useUIStore = create<UIState>((set, get) => ({
  theme: getStoredTheme(),
  sidebarOpen: false,
  sidebarCollapsed: false,
  shellMode: 'simple',
  activeModule: null,
  configVersion: 0,

  toggleTheme: () => {
    const next = get().theme === 'light' ? 'dark' : 'light'
    applyTheme(next)
    set({ theme: next })
  },

  setTheme: (theme) => {
    applyTheme(theme)
    set({ theme })
  },

  toggleSidebar: () => set((s) => ({ sidebarOpen: !s.sidebarOpen })),
  setSidebarOpen: (open) => set({ sidebarOpen: open }),
  setSidebarCollapsed: (collapsed) => set({ sidebarCollapsed: collapsed }),
  setShellMode: (mode) => set({ shellMode: mode }),
  setActiveModule: (module) => set({ activeModule: module }),
  setConfigVersion: (version) => set({ configVersion: version }),
}))

// Apply initial theme.
if (typeof document !== 'undefined') {
  applyTheme(getStoredTheme())
}
