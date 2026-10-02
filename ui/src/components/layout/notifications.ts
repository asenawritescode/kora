import type { RealtimeEvent } from '@/lib/realtime'

export const MAX_VISIBLE_NOTIFICATIONS = 30

export function prependNotification(current: RealtimeEvent[], notification: RealtimeEvent): RealtimeEvent[] {
  return [notification, ...current].slice(0, MAX_VISIBLE_NOTIFICATIONS)
}

export function subscribeToRealtimeNotifications(onNotification: (notification: RealtimeEvent) => void): () => void {
  const handleNotification = (event: Event) => {
    const detail = (event as CustomEvent<RealtimeEvent>).detail
    if (detail) onNotification(detail)
  }

  window.addEventListener('kora:realtime-notification', handleNotification as EventListener)
  return () => window.removeEventListener('kora:realtime-notification', handleNotification as EventListener)
}
