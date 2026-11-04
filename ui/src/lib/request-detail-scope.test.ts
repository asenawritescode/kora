import { describe, expect, it } from 'vitest'
import { requestDetailScope } from './request-detail-scope'

describe('request detail state scope', () => {
  it('separates same-ID requests by tenant and actor', () => {
    expect(requestDetailScope('/s/acme', 'alice', 'req-1')).not.toBe(requestDetailScope('/s/other', 'alice', 'req-1'))
    expect(requestDetailScope('/s/acme', 'alice', 'req-1')).not.toBe(requestDetailScope('/s/acme', 'bob', 'req-1'))
  })
})
