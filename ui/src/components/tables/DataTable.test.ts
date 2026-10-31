import { describe, expect, it } from 'vitest'
import { resolveUserDirectoryLabel } from './DataTable'

describe('embedded record table user labels', () => {
  it('uses the workspace display name instead of the stored user ID', () => {
    expect(resolveUserDirectoryLabel('user-1', [
      { name: 'user-1', full_name: 'Alice Example', email: 'alice@example.test' },
    ])).toBe('Alice Example')
  })

  it('hides an unavailable user ID behind a readable fallback', () => {
    expect(resolveUserDirectoryLabel('user-1', [])).toBe('Workspace user unavailable')
  })
})
