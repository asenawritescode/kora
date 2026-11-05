type PaymentSessionKey = 'kora-pos-active-shift' | 'kora-pos-active-till-session' | 'kora-workflow-prerequisites'

export function readPaymentSession(key: PaymentSessionKey): string {
  if (typeof window === 'undefined') return ''
  try {
    return window.sessionStorage.getItem(key) || ''
  } catch {
    return ''
  }
}

export function writePaymentSession(key: PaymentSessionKey, value: string): boolean {
  if (typeof window === 'undefined') return false
  try {
    window.sessionStorage.setItem(key, value)
    return true
  } catch {
    return false
  }
}
