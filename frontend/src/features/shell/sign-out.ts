import type { QueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'

/**
 * Revokes the current server session, then drops every cached identity so the
 * next page cannot render the previous user's data, and finally sends the
 * browser home.
 *
 * The caller acts on `false` only: a failed revoke leaves the HttpOnly cookie
 * (and therefore the session) alive, so pretending the user signed out would
 * be a lie. `navigate` is injected so the full-page browser hop can be
 * observed without loading a real document.
 */
export async function performSignOut(
  client: QueryClient,
  navigate: (href: string) => void = (href) => window.location.assign(href),
): Promise<boolean> {
  try {
    await apiClient.POST('/api/logout')
  } catch {
    return false
  }
  client.clear()
  navigate('/')
  return true
}
