import { getSitePrefix } from './basepath'

const SITE_SCOPE = getSitePrefix() || '_default'
const DB_NAME = `kora_document_drafts_${SITE_SCOPE}`
const DB_VERSION = 1
const STORE_NAME = 'drafts'
const META_STORAGE_KEY = `kora_document_draft_meta_${SITE_SCOPE}`
const FALLBACK_STORAGE_KEY = `kora_document_draft_fallback_${SITE_SCOPE}`

export type DraftDocumentName = string | undefined

export type DraftScope = {
  doctype: string
  name?: DraftDocumentName
}

export type DocumentDraft<TValue = Record<string, unknown>> = DraftScope & {
  value: TValue
  createdAt: string
  updatedAt: string
}

export type DocumentDraftMeta = DraftScope & {
  createdAt: string
  updatedAt: string
}

export type DraftListScope = Partial<DraftScope>

type DraftStore = Record<string, DocumentDraft<unknown>>
type DraftMetaStore = Record<string, DocumentDraftMeta>

export class DraftPersistenceError extends Error {
  constructor(message: string, readonly draftStored: boolean) {
    super(message)
    this.name = 'DraftPersistenceError'
  }
}

function draftKey(scope: DraftScope): string {
  return `${scope.doctype}:${scope.name || '__new__'}`
}

function safeReadJson<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return fallback
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as T) : fallback
  } catch {
    return fallback
  }
}

function safeWriteJson(key: string, value: unknown): boolean {
  try {
    localStorage.setItem(key, JSON.stringify(value))
    return true
  } catch {
    return false
  }
}

function readMetaStore(): DraftMetaStore {
  return safeReadJson<DraftMetaStore>(META_STORAGE_KEY, {})
}

function writeMetaStore(store: DraftMetaStore): boolean {
  return safeWriteJson(META_STORAGE_KEY, store)
}

function readFallbackStore(): DraftStore {
  return safeReadJson<DraftStore>(FALLBACK_STORAGE_KEY, {})
}

function writeFallbackStore(store: DraftStore): boolean {
  return safeWriteJson(FALLBACK_STORAGE_KEY, store)
}

function isDocumentDraftMeta(draft: unknown): draft is DocumentDraftMeta {
  return Boolean(
    draft &&
      typeof draft === 'object' &&
      typeof (draft as DocumentDraftMeta).doctype === 'string' &&
      typeof (draft as DocumentDraftMeta).createdAt === 'string' &&
      typeof (draft as DocumentDraftMeta).updatedAt === 'string',
  )
}

function metaRecordFromDraft(draft: DocumentDraft<unknown>): DocumentDraftMeta {
  return {
    doctype: draft.doctype,
    name: draft.name,
    createdAt: draft.createdAt,
    updatedAt: draft.updatedAt,
  }
}

async function openDb(): Promise<IDBDatabase | null> {
  if (typeof indexedDB === 'undefined') return null

  return await new Promise((resolve) => {
    const request = indexedDB.open(DB_NAME, DB_VERSION)

    request.onupgradeneeded = () => {
      const db = request.result
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME, { keyPath: 'key' })
      }
    }

    request.onsuccess = () => resolve(request.result)
    request.onerror = () => resolve(null)
    request.onblocked = () => resolve(null)
  })
}

async function readDraftRecord(key: string): Promise<DocumentDraft<unknown> | null> {
  const db = await openDb()
  if (!db) {
    const fallback = readFallbackStore()[key]
    return fallback ?? null
  }

  return await new Promise((resolve) => {
    const tx = db.transaction(STORE_NAME, 'readonly')
    const store = tx.objectStore(STORE_NAME)
    const request = store.get(key)
    request.onsuccess = () => {
      const result = request.result as { value?: DocumentDraft<unknown> } | undefined
      resolve(result?.value ?? null)
    }
    request.onerror = () => resolve(readFallbackStore()[key] ?? null)
  })
}

async function writeDraftRecord(key: string, draft: DocumentDraft<unknown>): Promise<boolean> {
  const db = await openDb()
  if (!db) {
    const store = readFallbackStore()
    store[key] = draft
    return writeFallbackStore(store)
  }

  return await new Promise((resolve) => {
    let settled = false
    const fallback = () => {
      if (settled) return
      settled = true
      const store = readFallbackStore()
      store[key] = draft
      resolve(writeFallbackStore(store))
    }
    try {
      const tx = db.transaction(STORE_NAME, 'readwrite')
      tx.objectStore(STORE_NAME).put({ key, value: draft })
      tx.oncomplete = () => {
        if (settled) return
        settled = true
        resolve(true)
      }
      tx.onabort = fallback
      tx.onerror = fallback
    } catch {
      fallback()
    }
  })
}

async function deleteDraftRecord(key: string): Promise<boolean> {
  const db = await openDb()
  if (!db) {
    const store = readFallbackStore()
    if (!store[key]) return false
    delete store[key]
    if (!writeFallbackStore(store)) {
      throw new DraftPersistenceError('The browser draft could not be deleted.', false)
    }
    return true
  }

  return await new Promise((resolve, reject) => {
    let settled = false
    const fallback = () => {
      if (settled) return
      settled = true
      const store = readFallbackStore()
      if (!store[key]) {
        reject(new DraftPersistenceError('The browser draft could not be confirmed as deleted.', false))
        return
      }
      delete store[key]
      if (!writeFallbackStore(store)) {
        reject(new DraftPersistenceError('The browser draft could not be deleted.', false))
        return
      }
      resolve(true)
    }
    try {
      const tx = db.transaction(STORE_NAME, 'readwrite')
      tx.objectStore(STORE_NAME).delete(key)
      tx.oncomplete = () => {
        if (settled) return
        settled = true
        resolve(true)
      }
      tx.onabort = fallback
      tx.onerror = fallback
    } catch {
      fallback()
    }
  })
}

function persistMeta(draft: DocumentDraftMeta): boolean {
  const metaStore = readMetaStore()
  metaStore[draftKey(draft)] = draft
  return writeMetaStore(metaStore)
}

function deleteMeta(scope: DraftScope): boolean {
  const metaStore = readMetaStore()
  const key = draftKey(scope)
  if (!metaStore[key]) return false
  delete metaStore[key]
  return writeMetaStore(metaStore)
}

export async function saveDocumentDraft<TValue = Record<string, unknown>>(
  scope: DraftScope,
  value: TValue,
): Promise<DocumentDraft<TValue>> {
  const key = draftKey(scope)
  const existing = await loadDocumentDraft(scope)
  const now = new Date().toISOString()
  const draft: DocumentDraft<TValue> = {
    doctype: scope.doctype,
    name: scope.name,
    value,
    createdAt: existing?.createdAt || now,
    updatedAt: now,
  }

  const stored = await writeDraftRecord(key, draft)
  if (!stored) {
    throw new DraftPersistenceError('The draft could not be saved in this browser.', false)
  }
  if (!persistMeta(metaRecordFromDraft(draft))) {
    throw new DraftPersistenceError('The draft was saved, but its local draft list could not be updated.', true)
  }

  return draft
}

export async function loadDocumentDraft<TValue = Record<string, unknown>>(
  scope: DraftScope,
): Promise<DocumentDraft<TValue> | null> {
  const draft = await readDraftRecord(draftKey(scope))
  return draft ? (draft as DocumentDraft<TValue>) : null
}

export async function clearDocumentDraft(scope: DraftScope): Promise<boolean> {
  const key = draftKey(scope)
  const deleted = await deleteDraftRecord(key)
  if (!deleteMeta(scope)) {
    throw new DraftPersistenceError('The draft was deleted, but its local draft list could not be updated.', deleted)
  }
  return deleted
}

export function listDocumentDrafts(scope: DraftListScope = {}): DocumentDraftMeta[] {
  return Object.values(readMetaStore())
    .filter(isDocumentDraftMeta)
    .filter((draft) => !scope.doctype || draft.doctype === scope.doctype)
    .filter((draft) => !scope.name || draft.name === scope.name)
    .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
}

export async function getDocumentDraftValue<TValue = Record<string, unknown>>(
  scope: DraftScope,
): Promise<TValue | null> {
  const draft = await loadDocumentDraft<TValue>(scope)
  return draft?.value ?? null
}

export function hasDraftMetadata(scope: DraftScope): boolean {
  return Boolean(readMetaStore()[draftKey(scope)])
}
