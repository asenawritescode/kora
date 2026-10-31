export function readOnboardingConversationID(): string | null {
  try {
    return window.sessionStorage.getItem('kora_onboarding_conversation')
  } catch {
    return null
  }
}

export function saveOnboardingConversationID(id: string): boolean {
  try {
    window.sessionStorage.setItem('kora_onboarding_conversation', id)
    return true
  } catch {
    return false
  }
}
