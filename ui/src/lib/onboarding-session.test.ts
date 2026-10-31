import { afterEach, describe, expect, it, vi } from 'vitest'
import { readOnboardingConversationID, saveOnboardingConversationID } from './onboarding-session'

describe('onboarding session persistence', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('fails safely when session storage is blocked', () => {
    vi.stubGlobal('window', {
      sessionStorage: {
        getItem: () => { throw new Error('blocked') },
        setItem: () => { throw new Error('blocked') },
      },
    })
    expect(readOnboardingConversationID()).toBeNull()
    expect(saveOnboardingConversationID('conversation-1')).toBe(false)
  })
})
