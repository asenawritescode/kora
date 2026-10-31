import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { readPaymentSession, writePaymentSession } from './payment-session'

describe('payment session storage', () => {
  beforeEach(() => {
    const values = new Map<string, string>()
    vi.stubGlobal('window', { sessionStorage: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => { values.set(key, value) },
    } })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('reads and writes a session value', () => {
    expect(writePaymentSession('kora-pos-active-shift', 'shift-1')).toBe(true)
    expect(readPaymentSession('kora-pos-active-shift')).toBe('shift-1')
  })

  it('returns safe defaults when session storage is blocked', () => {
    vi.stubGlobal('window', {
      get sessionStorage() { throw new Error('access denied') },
    })
    expect(readPaymentSession('kora-pos-active-shift')).toBe('')
    expect(writePaymentSession('kora-pos-active-shift', 'shift-1')).toBe(false)
  })

  it('reports quota and security failures while writing', () => {
    vi.stubGlobal('window', {
      sessionStorage: {
        getItem: () => null,
        setItem: () => { throw new Error('quota exceeded') },
      },
    })
    expect(writePaymentSession('kora-pos-active-till-session', 'till-1')).toBe(false)
  })
})
