import { describe, expect, it } from 'vitest'
import { firstCursorPage, nextCursorPage, previousCursorPage } from './cursor-page'

describe('cursor page state', () => {
  it('walks forward and back without losing the first page', () => {
    const first = firstCursorPage()
    const second = nextCursorPage(first, 'cursor-25')
    const third = nextCursorPage(second, 'cursor-50')

    expect(second).toEqual({ cursor: 'cursor-25', previous: [undefined] })
    expect(third).toEqual({ cursor: 'cursor-50', previous: [undefined, 'cursor-25'] })
    expect(previousCursorPage(third)).toEqual(second)
    expect(previousCursorPage(second)).toEqual(first)
    expect(previousCursorPage(first)).toBe(first)
  })

  it('does not advance without a server-provided next cursor', () => {
    const first = firstCursorPage()
    expect(nextCursorPage(first)).toBe(first)
  })
})
