import { describe, expect, it } from 'vitest'
import {
  isAllowedActionCommand,
  isAllowedResourceQuery,
  isReservedManifestRoute,
  isSafeBindingPath,
  isSafeManifestRoute,
  manifestRouteToPageSegment,
  normalizeManifestRoute,
  resourcePolicyError,
} from './policy'
import type { PageResource } from '@/manifest/schema/page'

describe('manifest runtime policy', () => {
  it('normalizes backend manifest routes without hardcoding the workspace basepath', () => {
    expect(normalizeManifestRoute('pos')).toBe('/pos')
    expect(manifestRouteToPageSegment('/pos')).toBe('pos')
  })

  it('blocks routes that collide with Kigae core namespaces', () => {
    expect(isReservedManifestRoute('/workspace/admin/page-manifests')).toBe(true)
    expect(isReservedManifestRoute('/pos')).toBe(false)
    expect(isSafeManifestRoute('/pos')).toBe(true)
    expect(isSafeManifestRoute('/pos?debug=true')).toBe(false)
  })

  it('allowlists runtime resource queries and action commands', () => {
    expect(isAllowedResourceQuery('document.list')).toBe(true)
    expect(isAllowedResourceQuery('sql.raw')).toBe(false)
    expect(isAllowedActionCommand('document.create')).toBe(true)
    expect(isAllowedActionCommand('javascript.eval')).toBe(false)
  })

  it('treats bindings as data paths, not executable expressions', () => {
    expect(isSafeBindingPath('orders.data')).toBe(true)
    expect(isSafeBindingPath('customer.address.city')).toBe(true)
    expect(isSafeBindingPath('orders.data[0]')).toBe(false)
    expect(isSafeBindingPath('orders.data;alert(1)')).toBe(false)
  })

  it('bounds database list filters and visible-name search before execution', () => {
    const resource: PageResource = {
      id: 'deals',
      query: 'document.list',
      params: {
        doctype: 'Deal',
        limit: 50,
        order_by: 'modified DESC',
        search: 'Northwind',
        filters: [{ field: 'pipeline_stage', op: '=', value: 'Qualified' }],
      },
    }
    expect(resourcePolicyError(resource)).toBeNull()
    expect(resourcePolicyError({ ...resource, params: { ...resource.params, limit: 201 } })).toContain('limit')
    expect(resourcePolicyError({ ...resource, params: { ...resource.params, search: 'x'.repeat(129) } })).toContain('search')
    expect(resourcePolicyError({ ...resource, params: { ...resource.params, order_by: 'modified; DROP TABLE Deal' } })).toContain('order_by')
    expect(resourcePolicyError({ ...resource, params: { ...resource.params, filters: [{ field: 'pipeline_stage);--', op: '=', value: 'Qualified' }] } })).toContain('safe field names')
  })
})
