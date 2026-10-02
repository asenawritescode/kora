import { afterEach, describe, expect, it, vi } from 'vitest'
import type { RealtimeEvent } from '@/lib/realtime'
import { MAX_VISIBLE_NOTIFICATIONS, prependNotification, subscribeToRealtimeNotifications } from './notifications'

describe('notification bell event handling', () => {
  afterEach(() => {
    delete (globalThis as any).window
  })

  it('adds realtime notifications to the bell feed and unsubscribes on cleanup', () => {
    const listeners = new Map<string, Set<(event: Event) => void>>()
    const fakeWindow = {
      addEventListener(type: string, handler: (event: Event) => void) {
        const handlers = listeners.get(type) ?? new Set()
        handlers.add(handler)
        listeners.set(type, handlers)
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

    let feed: RealtimeEvent[] = []
    const onNotification = vi.fn((notification: RealtimeEvent) => {
      feed = prependNotification(feed, notification)
    })
    const cleanup = subscribeToRealtimeNotifications(onNotification)
    const detail: RealtimeEvent = {
      type: 'notification',
      title: 'Sale paid',
      message: 'Your payment was recorded.',
      severity: 'success',
      action: { label: 'Open sale', href: '/workspace/Sale/SALE-1' },
    }
    const event = new Event('kora:realtime-notification')
    Object.defineProperty(event, 'detail', { value: detail })
    fakeWindow.dispatchEvent(event)

    expect(onNotification).toHaveBeenCalledWith(detail)
    expect(feed).toEqual([detail])
    expect(feed[0].action).toEqual({ label: 'Open sale', href: '/workspace/Sale/SALE-1' })

    cleanup()
    fakeWindow.dispatchEvent(event)
    expect(onNotification).toHaveBeenCalledTimes(1)
  })

  it('keeps newest notifications first and caps the bell feed', () => {
    const existing = Array.from({ length: MAX_VISIBLE_NOTIFICATIONS }, (_, index) => ({
      type: 'notification',
      title: `Old ${index}`,
    })) as RealtimeEvent[]
    const newest: RealtimeEvent = { type: 'notification', title: 'Just happened' }

    const feed = prependNotification(existing, newest)

    expect(feed).toHaveLength(MAX_VISIBLE_NOTIFICATIONS)
    expect(feed[0]).toBe(newest)
    expect(feed).not.toContain(existing.at(-1))
  })
})
