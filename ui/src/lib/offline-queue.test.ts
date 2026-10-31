import { beforeEach, describe, expect, it, vi } from 'vitest'
import offlineQueueSource from './offline-queue.ts?raw'
import {
  clearOfflineQueue,
  enqueueOfflineItem,
  loadOfflineQueue,
  markOfflineItemStatus,
  recordOfflineConflict,
} from './offline-queue'

describe('offline queue', () => {
  beforeEach(async () => {
    vi.stubGlobal('indexedDB', undefined)
    await clearOfflineQueue()
  })

  it('queues, loads, and updates operation status without localStorage', async () => {
    const item = await enqueueOfflineItem({
      id: 'op-1',
      type: 'pos.sale',
      payload: { total: 123 },
      deviceId: 'device-1',
      baseVersion: 7,
    })

    expect(item.status).toBe('queued')
    expect(item.operationId).toBe('op-1')
    expect(await loadOfflineQueue()).toHaveLength(1)

    const updated = await markOfflineItemStatus('op-1', 'accepted')
    expect(updated[0].status).toBe('accepted')
  })

  it('records conflicts as visible queue entries', async () => {
    await enqueueOfflineItem({
      id: 'op-2',
      type: 'pos.sale',
      payload: { total: 456 },
    })

    const updated = await recordOfflineConflict('op-2', { code: 'stale_base_version', message: 'Server record changed.' })
    expect(updated[0]).toMatchObject({
      id: 'op-2',
      status: 'conflict',
      error: { code: 'stale_base_version' },
    })
  })

  it('does not use localStorage for the offline business queue', () => {
    expect(offlineQueueSource).not.toContain('localStorage')
  })

  it('rejects when IndexedDB reports a transaction abort after the request succeeds', async () => {
    const failure = new DOMException('quota exceeded', 'QuotaExceededError')
    vi.stubGlobal('indexedDB', indexedDBWithTransactionAbort(failure))

    await expect(enqueueOfflineItem({ id: 'op-abort', type: 'pos.sale', payload: { total: 10 } })).rejects.toBe(failure)
  })

  it('preserves the memory queue when clearing IndexedDB fails', async () => {
    vi.stubGlobal('indexedDB', undefined)
    await enqueueOfflineItem({ id: 'op-memory', type: 'pos.sale', payload: { total: 10 } })
    const failure = new DOMException('storage unavailable', 'UnknownError')
    vi.stubGlobal('indexedDB', indexedDBWithTransactionAbort(failure))

    await expect(clearOfflineQueue()).rejects.toBe(failure)
    vi.stubGlobal('indexedDB', undefined)
    await expect(loadOfflineQueue()).resolves.toMatchObject([{ id: 'op-memory' }])
  })

  it('rejects a blocked IndexedDB upgrade instead of silently treating the queue as empty', async () => {
    const request: Record<string, unknown> = {}
    vi.stubGlobal('indexedDB', {
      open: () => {
        queueMicrotask(() => (request.onblocked as (() => void) | undefined)?.())
        return request
      },
    } as unknown as IDBFactory)

    await expect(loadOfflineQueue()).rejects.toThrow('Offline queue database upgrade is blocked')
  })
})

function indexedDBWithTransactionAbort(error: DOMException): IDBFactory {
  const database = {
    close: () => undefined,
    transaction: () => {
      const tx: Record<string, unknown> = { error: null }
      tx.objectStore = () => ({
        put: () => {
          const request: Record<string, unknown> = { error: null, result: undefined }
          queueMicrotask(() => {
            ;(request.onsuccess as (() => void) | undefined)?.()
            tx.error = error
            ;(tx.onabort as (() => void) | undefined)?.()
          })
          return request
        },
        clear: () => {
          const request: Record<string, unknown> = { error: null, result: undefined }
          queueMicrotask(() => {
            ;(request.onsuccess as (() => void) | undefined)?.()
            tx.error = error
            ;(tx.onabort as (() => void) | undefined)?.()
          })
          return request
        },
      })
      return tx
    },
  }
  return {
    open: () => {
      const request: Record<string, unknown> = { result: database }
      queueMicrotask(() => (request.onsuccess as (() => void) | undefined)?.())
      return request
    },
  } as unknown as IDBFactory
}
