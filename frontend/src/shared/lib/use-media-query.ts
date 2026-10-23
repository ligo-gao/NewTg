import { useEffect, useState } from 'react'

/**
 * Subscribes to a media query. When the environment has no `matchMedia`
 * (jsdom without a polyfill, old embedded webviews), the query is treated as
 * matching so the console does not fail closed on a missing browser API.
 */
export function useMediaQuery(query: string): boolean {
  const read = (): boolean => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return true
    }
    return window.matchMedia(query).matches
  }

  const [matches, setMatches] = useState(read)

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return
    }
    const list = window.matchMedia(query)
    const update = (): void => setMatches(list.matches)
    update()
    list.addEventListener('change', update)
    return () => list.removeEventListener('change', update)
  }, [query])

  return matches
}
