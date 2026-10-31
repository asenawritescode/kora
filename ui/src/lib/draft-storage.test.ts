import { afterEach, describe, expect, it, vi } from 'vitest'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.resetModules()
})

function storageThatRejects(rejectedKey: (key: string) => boolean) {
  const values = new Map<string, string>()
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      if (rejectedKey(key)) throw new DOMException('quota exceeded', 'QuotaExceededError')
      values.set(key, value)
    },
    removeItem: (key: string) => values.delete(key),
    values,
  }
}

describe('document draft persistence', () => {
  it('reports when the draft itself cannot be stored', async () => {
    vi.stubGlobal('window', { location: { pathname: '/s/test' } })
    vi.stubGlobal('indexedDB', undefined)
    vi.stubGlobal('localStorage', storageThatRejects((key) => key.includes('_fallback_')))
    const { DraftPersistenceError, saveDocumentDraft } = await import('./draft-storage')

    await expect(saveDocumentDraft({ doctype: 'Invoice' }, { amount: 42 })).rejects.toMatchObject({
      name: DraftPersistenceError.name,
      draftStored: false,
    })
  })

  it('reports a saved draft when only the draft index write fails', async () => {
    vi.stubGlobal('window', { location: { pathname: '/s/test' } })
    vi.stubGlobal('indexedDB', undefined)
    const localStorage = storageThatRejects((key) => key.includes('_meta_'))
    vi.stubGlobal('localStorage', localStorage)
    const { DraftPersistenceError, loadDocumentDraft, saveDocumentDraft } = await import('./draft-storage')

    await expect(saveDocumentDraft({ doctype: 'Invoice' }, { amount: 42 })).rejects.toMatchObject({
      name: DraftPersistenceError.name,
      draftStored: true,
    })
    await expect(loadDocumentDraft({ doctype: 'Invoice' })).resolves.toMatchObject({
      value: { amount: 42 },
    })
  })

  it('reports when clearing an existing fallback draft cannot be persisted', async () => {
    vi.stubGlobal('window', { location: { pathname: '/s/test' } })
    vi.stubGlobal('indexedDB', undefined)
    let rejectFallbackWrite = false
    vi.stubGlobal('localStorage', storageThatRejects((key) => rejectFallbackWrite && key.includes('_fallback_')))
    const { DraftPersistenceError, clearDocumentDraft, saveDocumentDraft } = await import('./draft-storage')
    await saveDocumentDraft({ doctype: 'Invoice' }, { amount: 42 })

    rejectFallbackWrite = true
    await expect(clearDocumentDraft({ doctype: 'Invoice' })).rejects.toMatchObject({
      name: DraftPersistenceError.name,
      draftStored: false,
    })
  })
})
