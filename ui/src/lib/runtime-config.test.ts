import { afterEach, describe, expect, it, vi } from 'vitest'
import { loadRuntimeConfig } from './runtime-config'

describe('runtime config', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('falls back to embedded defaults', () => {
    const cfg = loadRuntimeConfig()
    expect(cfg.apiBaseUrl).toBe('/api')
    expect(cfg.realtimeBaseUrl).toBe('/api/v1/system/realtime')
    expect(cfg.environment).toBe('embedded')
  })

  it('reads injected runtime config from the bootstrap script', () => {
    const doc = {
      getElementById: (id: string) => {
        if (id !== 'kora-runtime-config') {
          return null
        }
        return {
          tagName: 'SCRIPT',
          textContent: '{"apiBaseUrl":"https://example.test/api","appName":"Standalone Kigae","environment":"standalone","buildVersion":"1.2.3"}',
        }
      },
    }
    vi.stubGlobal('document', doc)
    vi.stubGlobal('window', { __KORA_UI_CONFIG__: {} })

    const cfg = loadRuntimeConfig()
    expect(cfg.apiBaseUrl).toBe('https://example.test/api')
    expect(cfg.appName).toBe('Standalone Kigae')
    expect(cfg.environment).toBe('standalone')
    expect(cfg.buildVersion).toBe('1.2.3')
  })
})
