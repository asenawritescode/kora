export interface RuntimeConfig {
  apiBaseUrl: string
  realtimeBaseUrl?: string
  appName: string
  environment: string
  buildVersion?: string
  capabilitiesVersion?: string
  capabilities?: string[]
  offlineMode?: 'unsupported' | 'read_only' | 'queue_writes' | 'full_slice'
}

const OFFLINE_MODES = new Set(['unsupported', 'read_only', 'queue_writes', 'full_slice'])

declare global {
  interface Window {
    __KORA_UI_CONFIG__?: Partial<RuntimeConfig>
  }
}

export function loadRuntimeConfig(): RuntimeConfig {
  const injected = readInjectedRuntimeConfig()
  const apiBaseUrl = readString(injected.apiBaseUrl, '/api')
  const realtimeBaseUrl = readOptionalString(injected.realtimeBaseUrl) || deriveRealtimeBaseUrl(apiBaseUrl)
  const offlineMode = OFFLINE_MODES.has(String(injected.offlineMode))
    ? injected.offlineMode
    : 'unsupported'

  return {
    apiBaseUrl,
    realtimeBaseUrl,
    appName: readString(injected.appName, 'Kigae'),
    environment: readString(injected.environment, 'embedded'),
    buildVersion: readOptionalString(injected.buildVersion),
    capabilitiesVersion: readOptionalString(injected.capabilitiesVersion),
    capabilities: readStringArray(injected.capabilities),
    offlineMode,
  }
}

export function runtimeSupportsCapability(config: RuntimeConfig, capability: string): boolean {
  return config.capabilities?.includes(capability) ?? false
}

export function runtimeAllowsOffline(
  config: RuntimeConfig,
  requested: RuntimeConfig['offlineMode'],
): boolean {
  const rank = {
    unsupported: 0,
    read_only: 1,
    queue_writes: 2,
    full_slice: 3,
  } satisfies Record<NonNullable<RuntimeConfig['offlineMode']>, number>

  return rank[config.offlineMode || 'unsupported'] >= rank[requested || 'unsupported']
}

function deriveRealtimeBaseUrl(apiBaseUrl: string): string {
  const normalized = apiBaseUrl.replace(/\/$/, '')
  if (!normalized) {
    return '/api/v1/system/realtime'
  }
  return `${normalized}/v1/system/realtime`
}

function readInjectedRuntimeConfig(): Partial<RuntimeConfig> {
  if (typeof window === 'undefined') {
    return {}
  }
  const inline = window.__KORA_UI_CONFIG__ || {}
  const script = document.getElementById('kora-runtime-config')
  if (!script || script.tagName !== 'SCRIPT') {
    return inline
  }
  try {
    const parsed = JSON.parse(script.textContent || '{}') as Partial<RuntimeConfig>
    return { ...inline, ...parsed }
  } catch {
    return inline
  }
}

function readString(value: unknown, fallback: string): string {
  return typeof value === 'string' && value.trim() ? value : fallback
}

function readOptionalString(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value : undefined
}

function readStringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value.filter((entry): entry is string => typeof entry === 'string' && entry.trim().length > 0)
}
