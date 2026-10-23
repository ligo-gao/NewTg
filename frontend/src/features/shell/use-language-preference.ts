import { useQuery, useQueryClient } from '@tanstack/react-query'

import {
  selfSettingsQueryOptions,
  SELF_SETTINGS_QUERY_KEY,
  updateLanguagePreference,
} from '@/lib/self-settings'
import type { Locale } from '@/shared/lib/i18n-context'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

export interface LanguagePreference {
  /**
   * Persists the locale for a signed-in session. Anonymous visitors get
   * `undefined` so `LanguageSwitch` only changes the locale locally and never
   * calls `/api/self` (which would answer 401).
   */
  onChange?: (locale: Locale) => void
}

/**
 * Console language persistence (moved out of the removed `ConsoleSiteNav`).
 *
 * Signed-in users save the choice to their profile; the endpoint uses an
 * optimistic lock, so the current `updated_at` is required. A failed save
 * restores the previous locale and surfaces the existing toast copy.
 */
export function useLanguagePreference(signedIn: boolean): LanguagePreference {
  const { t, locale, setLocale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  // `enabled: false` keeps the observer subscribed to the cache without firing
  // a request on mount: LocalePreferenceSync already reads /api/self for a
  // signed-in session, and anonymous visitors must not hit the endpoint.
  const selfQuery = useQuery({
    ...selfSettingsQueryOptions(),
    enabled: false,
    queryKey: SELF_SETTINGS_QUERY_KEY,
  })

  async function persist(next: Locale, previous: Locale): Promise<void> {
    try {
      // The endpoint uses an optimistic lock, so the current `updated_at` is
      // required. Fetch once if the sync has not populated the cache yet.
      let current = selfQuery.data
      if (current === undefined) {
        current = (await selfQuery.refetch()).data
      }
      if (current === undefined) {
        push({ tone: 'info', message: t('common.language.saved_locally') })
        return
      }
      const updated = await updateLanguagePreference(next, current.updated_at)
      // Write the fresh settings back so LocalePreferenceSync cannot replay the
      // old language over the one the user just picked.
      queryClient.setQueryData(SELF_SETTINGS_QUERY_KEY, updated)
    } catch {
      // A failed save must not leave the UI and the profile on different
      // languages; restore the previous one and invalidate the stale lock.
      setLocale?.(previous)
      push({ tone: 'error', message: t('common.language.update_error') })
      await queryClient.invalidateQueries({ queryKey: SELF_SETTINGS_QUERY_KEY })
    }
  }

  return {
    onChange: signedIn
      ? (next: Locale) => {
          void persist(next, locale)
        }
      : undefined,
  }
}
