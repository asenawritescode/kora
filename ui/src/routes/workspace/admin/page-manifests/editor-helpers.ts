import type { PageAction, PageManifest, PageResource } from '../../../../manifest/schema/page'
import { validatePageManifestContract } from '../../../../manifest/schema/page'
import { isAllowedActionCommand, isAllowedResourceQuery } from '../../../../manifest/runtime/policy'

export type PickerOption = { value: string; label: string; description?: string }
export type PreviewViewport = 'desktop' | 'tablet' | 'mobile'

export type ManifestDraftSnapshot = {
  manifest: PageManifest
  savedAt: string
  source: 'editor' | 'source'
}

export function buildResourceBindingOptions(manifest: PageManifest): PickerOption[] {
  return manifest.spec.resources.map((resource) => ({
    value: `${resource.id}.data`,
    label: resource.id,
    description: resourceSummary(resource),
  }))
}

export function buildActionBindingOptions(manifest: PageManifest): PickerOption[] {
  return manifest.spec.actions.map((action) => ({
    value: action.id,
    label: action.id,
    description: actionSummary(action),
  }))
}

export function buildPublishPreflight(manifest: PageManifest): {
  canPublish: boolean
  issues: ReturnType<typeof validatePageManifestContract>
  resourceCount: number
  actionCount: number
  unsupportedResources: string[]
  unsupportedActions: string[]
} {
  const issues = validatePageManifestContract(manifest)
  const unsupportedResources = manifest.spec.resources
    .filter((resource) => !isAllowedResourceQuery(resource.query))
    .map((resource) => resource.id)
  const unsupportedActions = manifest.spec.actions
    .filter((action) => !isAllowedActionCommand(action.command))
    .map((action) => action.id)

  return {
    canPublish: issues.length === 0 && unsupportedResources.length === 0 && unsupportedActions.length === 0,
    issues,
    resourceCount: manifest.spec.resources.length,
    actionCount: manifest.spec.actions.length,
    unsupportedResources,
    unsupportedActions,
  }
}

export function manifestDraftStorageKey(name: string | undefined | null): string {
  return `kora_page_manifest_draft:${String(name || 'new')}`
}

export function readManifestDraft(name: string | undefined | null): ManifestDraftSnapshot | null {
  if (typeof window === 'undefined') return null
  const raw = localStorage.getItem(manifestDraftStorageKey(name))
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as ManifestDraftSnapshot
    if (!parsed || typeof parsed !== 'object' || !parsed.manifest || typeof parsed.savedAt !== 'string') return null
    return parsed
  } catch {
    return null
  }
}

export function writeManifestDraft(name: string | undefined | null, snapshot: ManifestDraftSnapshot): void {
  if (typeof window === 'undefined') return
  localStorage.setItem(manifestDraftStorageKey(name), JSON.stringify(snapshot))
}

export function clearManifestDraft(name: string | undefined | null): void {
  if (typeof window === 'undefined') return
  localStorage.removeItem(manifestDraftStorageKey(name))
}

export function previewViewportOptions(): PickerOption[] {
  return [
    { value: 'desktop', label: 'Desktop', description: 'Full-width preview' },
    { value: 'tablet', label: 'Tablet', description: 'Mid-width preview' },
    { value: 'mobile', label: 'Mobile', description: 'Narrow preview' },
  ]
}

export function previewViewportClass(viewport: PreviewViewport): string {
  switch (viewport) {
    case 'tablet':
      return 'mx-auto w-full max-w-4xl'
    case 'mobile':
      return 'mx-auto w-full max-w-sm'
    case 'desktop':
    default:
      return 'w-full'
  }
}

function resourceSummary(resource: PageResource): string {
  return `${resource.query} ${resource.params?.doctype ? `for ${String(resource.params.doctype)}` : ''}`.trim()
}

function actionSummary(action: PageAction): string {
  return `${action.command}${action.invalidate.length > 0 ? ` → ${action.invalidate.join(', ')}` : ''}`
}
