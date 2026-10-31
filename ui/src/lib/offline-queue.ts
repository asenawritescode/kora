export interface OfflineQueueItem<T = Record<string, unknown>> {
  id: string
  createdAt: string
  updatedAt?: string
  type: string
  payload: T
  status: 'queued' | 'syncing' | 'accepted' | 'rejected' | 'conflict' | 'discarded'
  site?: string
  branchId?: string
  deviceId?: string
  operationId?: string
  baseVersion?: number
  correlationId?: string
  error?: { code: string; message: string }
}

const DB_NAME = 'kora_offline_runtime'
const DB_VERSION = 1
const STORE_NAME = 'operation_queue'

const memoryQueue = new Map<string, OfflineQueueItem>()

export async function loadOfflineQueue(): Promise<OfflineQueueItem[]> {
  const db = await openOfflineDb()
  if (!db) return Array.from(memoryQueue.values()).sort(sortByCreatedAt)

  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, 'readonly')
    const request = tx.objectStore(STORE_NAME).getAll()
    let result: OfflineQueueItem[] | undefined
    request.onsuccess = () => { result = request.result as OfflineQueueItem[] }
    request.onerror = () => reject(request.error)
    tx.oncomplete = () => {
      if (!result) reject(new Error('Offline queue read completed without a result'))
      else resolve(result.sort(sortByCreatedAt))
    }
    tx.onerror = () => reject(tx.error ?? new Error('Offline queue read failed'))
    tx.onabort = () => reject(tx.error ?? new Error('Offline queue read was aborted'))
  })
}

export async function enqueueOfflineItem<T>(
  item: Omit<OfflineQueueItem<T>, 'status' | 'createdAt' | 'updatedAt'>,
): Promise<OfflineQueueItem<T>> {
  const queued: OfflineQueueItem<T> = {
    ...item,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    operationId: item.operationId || item.id,
    status: 'queued',
  }
  await putOfflineItem(queued as OfflineQueueItem)
  return queued
}

export async function markOfflineItemStatus(
  id: string,
  status: OfflineQueueItem['status'],
): Promise<OfflineQueueItem[]> {
  const queue = await loadOfflineQueue()
  const item = queue.find((entry) => entry.id === id)
  if (!item) return queue
  await putOfflineItem({ ...item, status, updatedAt: new Date().toISOString() })
  return loadOfflineQueue()
}

export async function recordOfflineConflict(
  id: string,
  error: { code: string; message: string },
): Promise<OfflineQueueItem[]> {
  const queue = await loadOfflineQueue()
  const item = queue.find((entry) => entry.id === id)
  if (!item) return queue
  await putOfflineItem({ ...item, status: 'conflict', error, updatedAt: new Date().toISOString() })
  return loadOfflineQueue()
}

export async function clearOfflineQueue(): Promise<void> {
  const db = await openOfflineDb()
  if (!db) {
    memoryQueue.clear()
    return
  }
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, 'readwrite')
    const request = tx.objectStore(STORE_NAME).clear()
    tx.oncomplete = () => {
      memoryQueue.clear()
      resolve()
    }
    request.onerror = () => reject(request.error)
    tx.onerror = () => reject(tx.error ?? new Error('Offline queue clear failed'))
    tx.onabort = () => reject(tx.error ?? new Error('Offline queue clear was aborted'))
  })
}

async function putOfflineItem(item: OfflineQueueItem): Promise<void> {
  const db = await openOfflineDb()
  if (!db) {
    memoryQueue.set(item.id, item)
    return
  }

  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, 'readwrite')
    const request = tx.objectStore(STORE_NAME).put(item)
    request.onerror = () => reject(request.error)
    tx.oncomplete = () => resolve()
    tx.onerror = () => reject(tx.error ?? new Error('Offline queue write failed'))
    tx.onabort = () => reject(tx.error ?? new Error('Offline queue write was aborted'))
  })
}

function openOfflineDb(): Promise<IDBDatabase | null> {
  if (typeof indexedDB === 'undefined') return Promise.resolve(null)

  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, DB_VERSION)
    let blocked = false
    request.onupgradeneeded = () => {
      const db = request.result
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        const store = db.createObjectStore(STORE_NAME, { keyPath: 'id' })
        store.createIndex('type', 'type', { unique: false })
        store.createIndex('status', 'status', { unique: false })
        store.createIndex('operationId', 'operationId', { unique: false })
      }
    }
    request.onsuccess = () => {
      if (blocked) request.result.close()
      else {
        request.result.onversionchange = () => request.result.close()
        resolve(request.result)
      }
    }
    request.onerror = () => reject(request.error)
    request.onblocked = () => {
      blocked = true
      reject(new Error('Offline queue database upgrade is blocked'))
    }
  })
}

function sortByCreatedAt(a: OfflineQueueItem, b: OfflineQueueItem): number {
  return a.createdAt.localeCompare(b.createdAt)
}
