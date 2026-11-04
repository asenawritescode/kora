import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { QueryClient } from '@tanstack/react-query'
import { extractInvalidationKey, mapRealtimeInvalidation, attachRealtimeInvalidation, describeRealtimeState } from './realtime'

describe('realtime invalidation', () => {
  beforeEach(() => {
    vi.useRealTimers()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    delete (globalThis as any).window
    delete (globalThis as any).WebSocket
    delete (globalThis as any).EventSource
    delete (globalThis as any).navigator
    vi.restoreAllMocks()
  })

  it('maps doctypes to page and list invalidations', () => {
    expect(mapRealtimeInvalidation({ type: 'change', doctype: 'Sales Order' })).toEqual([
      { queryKey: ['view-data', 'Sales Order', true] },
      { queryKey: ['view-data', 'Sales Order', false] },
      { queryKey: ['page-manifest', '/workspace/Sales Order', undefined] },
      { queryKey: ['page-manifests'] },
    ])
  })

  it('derives operation invalidation keys', () => {
    expect(extractInvalidationKey({ type: 'change', operation_id: 'op-123' })).toBe('operation:op-123')
  })

  it('maps realtime connection states to typed live-page labels', () => {
    expect(describeRealtimeState({ state: 'connecting' })).toEqual({
      label: 'Connecting',
      tone: 'outline',
      detail: 'Live updates are connecting',
    })
    expect(describeRealtimeState({ state: 'connected', detail: 'ok' })).toEqual({
      label: 'Live',
      tone: 'default',
      detail: 'ok',
    })
    expect(describeRealtimeState({ state: 'offline' })).toEqual({
      label: 'Offline',
      tone: 'secondary',
      detail: 'Live updates are offline',
    })
  })

  it('transitions through offline and reconnecting states from browser events', () => {
    const states: Array<{ state: string; detail?: string }> = []
    const queryClient = { invalidateQueries: vi.fn() } as any

    const listeners = new Map<string, Set<(event: Event) => void>>()
    const fakeWindow = {
      setTimeout,
      clearTimeout,
      addEventListener(type: string, handler: (event: Event) => void) {
        const set = listeners.get(type) ?? new Set()
        set.add(handler)
        listeners.set(type, set)
      },
      removeEventListener(type: string, handler: (event: Event) => void) {
        listeners.get(type)?.delete(handler)
      },
      dispatchEvent(event: Event) {
        listeners.get(event.type)?.forEach((handler) => handler(event))
        return true
      },
    }

    ;(globalThis as any).window = fakeWindow
    ;(globalThis as any).WebSocket = vi.fn().mockImplementation(function (this: any) {
      this.readyState = 0
      this.close = vi.fn()
    })
    ;(globalThis as any).EventSource = class { close() {} }
    ;(globalThis as any).navigator = { onLine: true }

    const cleanup = attachRealtimeInvalidation('/api/v1/system/realtime', queryClient, (state) => {
      states.push(state)
    })

    listeners.get('offline')?.forEach((handler) => handler(new Event('offline')))
    listeners.get('online')?.forEach((handler) => handler(new Event('online')))

    expect(states).toEqual([
      { state: 'connecting' },
      { state: 'offline', detail: 'Browser offline' },
      { state: 'reconnecting', detail: 'Network restored' },
    ])

    cleanup()
  })

  it('invalidates live-page data and emits notifications for change events', () => {
    const states: Array<{ state: string; detail?: string }> = []
    const queryClient = { invalidateQueries: vi.fn() } as any
    const listeners = new Map<string, Set<(event: Event) => void>>()
    const notifications: any[] = []
    const fakeWindow = {
      setTimeout,
      clearTimeout,
      addEventListener(type: string, handler: (event: Event) => void) {
        const set = listeners.get(type) ?? new Set()
        set.add(handler)
        listeners.set(type, set)
      },
      removeEventListener(type: string, handler: (event: Event) => void) {
        listeners.get(type)?.delete(handler)
      },
      dispatchEvent(event: Event) {
        if (event.type === 'kora:realtime-notification') {
          notifications.push((event as CustomEvent).detail)
        }
        listeners.get(event.type)?.forEach((handler) => handler(event))
        return true
      },
    }

    ;(globalThis as any).window = fakeWindow
    ;(globalThis as any).WebSocket = vi.fn().mockImplementation(function (this: any) {
      this.readyState = 0
      this.close = vi.fn()
    })
    ;(globalThis as any).EventSource = class { close() {} }
    ;(globalThis as any).navigator = { onLine: true }

    const cleanup = attachRealtimeInvalidation('/api/v1/system/realtime', queryClient, (state) => {
      states.push(state)
    })

    const payload = {
      type: 'change',
      doctype: 'Sales Order',
      doc_name: 'SO-0001',
      operation_id: 'op-123',
      occurred_at: '2026-08-27T00:00:00.000Z',
    }
    const socket = (globalThis.WebSocket as any).mock.instances[0]
    socket.onmessage?.({ data: JSON.stringify(payload) })
    const workflowNotification = {
      type: 'notification',
      title: 'Sale paid',
      message: 'Your payment was recorded.',
      severity: 'success',
      action: { label: 'Open sale', href: '/workspace/Sale/SALE-1' },
    }
    socket.onmessage?.({ data: JSON.stringify(workflowNotification) })

    expect(queryClient.invalidateQueries).toHaveBeenCalledWith({ queryKey: ['view-data', 'Sales Order', true] })
    expect(queryClient.invalidateQueries).toHaveBeenCalledWith({ queryKey: ['view-data', 'Sales Order', false] })
    expect(queryClient.invalidateQueries).toHaveBeenCalledWith({ queryKey: ['page-manifest', '/workspace/Sales Order', undefined] })
    expect(queryClient.invalidateQueries).toHaveBeenCalledWith({ queryKey: ['page-manifests'] })
    expect(notifications[0]).toMatchObject({
      type: 'notification',
      title: 'Sales Order updated',
      message: 'Sales Order SO-0001 changed',
      action: { label: 'Open doctype', href: '/workspace/Sales%20Order' },
    })
    expect(notifications[1]).toMatchObject(workflowNotification)
    expect(states[0]).toEqual({ state: 'connecting' })
    expect(states.at(-1)).toMatchObject({ state: 'connected' })

    cleanup()
  })

  it('falls back to SSE and schedules reconnects when websocket startup fails', () => {
    const states: Array<{ state: string; detail?: string }> = []
    const queryClient = { invalidateQueries: vi.fn() } as any
    const timers: Array<() => void> = []
    const listeners = new Map<string, Set<(event: Event) => void>>()
    const fakeWindow = {
      setTimeout(handler: () => void) {
        timers.push(handler)
        return timers.length
      },
      clearTimeout() {},
      addEventListener(type: string, handler: (event: Event) => void) {
        const set = listeners.get(type) ?? new Set()
        set.add(handler)
        listeners.set(type, set)
      },
      removeEventListener(type: string, handler: (event: Event) => void) {
        listeners.get(type)?.delete(handler)
      },
      dispatchEvent(event: Event) {
        listeners.get(event.type)?.forEach((handler) => handler(event))
        return true
      },
    }

    const FakeEventSource = vi.fn().mockImplementation(function (this: any) {
      this.onopen = null
      this.onmessage = null
      this.onerror = null
      this.close = vi.fn()
      this.addEventListener = vi.fn()
    })

    ;(globalThis as any).window = fakeWindow
    ;(globalThis as any).WebSocket = vi.fn().mockImplementation(() => {
      throw new Error('ws failed')
    })
    ;(globalThis as any).EventSource = FakeEventSource
    ;(globalThis as any).navigator = { onLine: true }

    const cleanup = attachRealtimeInvalidation('/api/v1/system/realtime', queryClient, (state) => {
      states.push(state)
    })

    expect(states[0]).toEqual({ state: 'connecting' })

    const instance = FakeEventSource.mock.instances[0] as any
    instance.onerror?.()
    expect(states.at(-1)).toEqual({ state: 'reconnecting', detail: 'Realtime reconnecting' })
    expect(timers.length).toBeGreaterThan(0)

    cleanup()
  })

  it('closes the tenant stream and removes listeners when its scope is retired', () => {
    const queryClient = { invalidateQueries: vi.fn() } as unknown as QueryClient
    const listeners = new Map<string, Set<EventListener>>()
    const fakeWindow = {
      location: { href: 'http://localhost/s/acme/workspace', pathname: '/s/acme/workspace' } as Location,
      setTimeout: vi.fn(() => 1),
      clearTimeout: vi.fn(),
      addEventListener(type: string, handler: EventListener) {
        const handlers = listeners.get(type) ?? new Set()
        handlers.add(handler)
        listeners.set(type, handlers)
      },
      removeEventListener(type: string, handler: EventListener) {
        listeners.get(type)?.delete(handler)
      },
    }

    class FakeSocket {
      static instances: FakeSocket[] = []
      readonly url: string
      onclose: ((event: CloseEvent) => void) | null = null
      close = vi.fn(() => this.onclose?.({} as CloseEvent))
      constructor(url: string) {
        this.url = url
        FakeSocket.instances.push(this)
      }
    }

    vi.stubGlobal('window', fakeWindow as unknown as Window)
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    vi.stubGlobal('EventSource', class FakeEventSource {})
    vi.stubGlobal('navigator', { onLine: true } as Navigator)

    const cleanup = attachRealtimeInvalidation('/s/acme/api/v1/system/realtime', queryClient)
    const socket = FakeSocket.instances[0]
    expect(socket?.url).toBe('/s/acme/api/v1/system/realtime')
    expect(listeners.get('online')?.size).toBe(1)
    expect(listeners.get('offline')?.size).toBe(1)

    cleanup()

    expect(socket?.close).toHaveBeenCalledOnce()
    expect(listeners.get('online')?.size ?? 0).toBe(0)
    expect(listeners.get('offline')?.size ?? 0).toBe(0)
    expect(fakeWindow.setTimeout).not.toHaveBeenCalled()
  })
})
