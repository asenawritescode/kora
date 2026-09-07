import { validatePageManifestContract, type PageComponent, type PageManifest, type PageManifestValidationIssue } from '../schema/page'

export const GENERATED_COMPONENT_ALLOWLIST = [
  'dashboard_grid',
  'metric_card',
  'record_table',
  'filter_bar',
  'search_box',
  'franken_card',
  'franken_button',
  'franken_badge',
  'franken_input',
  'franken_table',
  'query_summary',
  'data_provenance',
  'data_quality',
  'comparison_metric',
  'refresh_state',
] as const

export type GeneratedComponentType = typeof GENERATED_COMPONENT_ALLOWLIST[number]

export interface GeneratedTenantConfig {
  tenantId: string
  theme: 'zinc' | 'slate' | 'stone' | 'gray'
  allowedComponents: readonly GeneratedComponentType[]
}

export interface GeneratedPageRevision {
  revision: number
  state: 'draft' | 'published'
  manifest: PageManifest
  tenantId: string
  updatedAt: string
}

export function validateGeneratedPage(
  manifest: PageManifest,
  tenant: GeneratedTenantConfig,
): PageManifestValidationIssue[] {
  const issues = validatePageManifestContract(manifest)
  const allowed = new Set(tenant.allowedComponents)
  visitComponents(manifest.spec.layout.children, (component, path) => {
    if (!allowed.has(component.component as GeneratedComponentType)) {
      issues.push({ path: `${path}.component`, message: `${component.component} is not enabled for generated pages.` })
    }
  })
  return issues
}

export function acceptGeneratedPage(
  manifest: PageManifest,
  tenant: GeneratedTenantConfig,
): { manifest: PageManifest | null; issues: PageManifestValidationIssue[] } {
  const issues = validateGeneratedPage(manifest, tenant)
  return issues.length ? { manifest: null, issues } : { manifest, issues: [] }
}

export function createGeneratedRevision(
  manifest: PageManifest,
  tenant: GeneratedTenantConfig,
  revision: number,
  state: GeneratedPageRevision['state'] = 'draft',
): GeneratedPageRevision {
  const accepted = acceptGeneratedPage(manifest, tenant)
  if (accepted.issues.length) throw new Error(accepted.issues.map((issue) => `${issue.path}: ${issue.message}`).join(' '))
  return {
    revision,
    state,
    manifest,
    tenantId: tenant.tenantId,
    updatedAt: new Date().toISOString(),
  }
}

function visitComponents(
  components: PageComponent[],
  visit: (component: PageComponent, path: string) => void,
  prefix = 'spec.layout.children',
) {
  components.forEach((component, index) => {
    const path = `${prefix}.${index}`
    visit(component, path)
    if (component.children?.length) visitComponents(component.children, visit, `${path}.children`)
  })
}
