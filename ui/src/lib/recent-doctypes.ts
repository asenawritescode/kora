import { getSitePrefix } from './basepath'

// Scope localStorage keys by site to prevent cross-site data leakage.
// Without site scoping, recents/favorites from one site would appear on
// another site (same origin), leading to 404s when clicking items that
// don't exist on the current site.
const SITE_SCOPE = getSitePrefix() || '_default'

const STORAGE_KEY = `kora_recent_doctypes_${SITE_SCOPE}`
const FAVORITES_KEY = `kora_favorite_doctypes_${SITE_SCOPE}`
const MAX_RECENT = 6

export type RecentDocType = { name: string; label: string; icon?: string }

export function getRecentDoctypes(): RecentDocType[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? JSON.parse(raw) : []
  } catch { return [] }
}

export function recordDoctypeVisit(dt: RecentDocType) {
  const recent = getRecentDoctypes().filter(r => r.name !== dt.name)
  recent.unshift(dt)
  localStorage.setItem(STORAGE_KEY, JSON.stringify(recent.slice(0, MAX_RECENT)))
}

export function getFavorites(): RecentDocType[] {
  try {
    const raw = localStorage.getItem(FAVORITES_KEY)
    return raw ? JSON.parse(raw) : []
  } catch { return [] }
}

export function toggleFavorite(dt: RecentDocType): boolean {
  const favs = getFavorites()
  const idx = favs.findIndex(f => f.name === dt.name)
  if (idx >= 0) {
    favs.splice(idx, 1)
    localStorage.setItem(FAVORITES_KEY, JSON.stringify(favs))
    return false // un-favorited
  } else {
    favs.push(dt)
    localStorage.setItem(FAVORITES_KEY, JSON.stringify(favs))
    return true // favorited
  }
}

export function isFavorite(name: string): boolean {
  return getFavorites().some(f => f.name === name)
}
