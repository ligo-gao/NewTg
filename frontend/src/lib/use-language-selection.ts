import { useQuery, useQueryClient } from '@tanstack/react-query'

import { persistLanguagePreference } from '@/lib/language-preference'
import {
  selfSettingsQueryOptions,
  SELF_SETTINGS_QUERY_KEY,
} from '@/lib/self-settings'
import type { Locale } from '@/shared/lib/i18n-context'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

export interface LanguageSelection {
  /**
   * Persists the locale for a signed-in session. Anonymous visitors get
   * `undefined` so `LanguageSwitch` only changes the locale locally and never
   * calls `/api/self` (which would answer 401).
   *
   * Returns a promise when a save actually runs, so a form control can disable
   * itself until the optimistic-lock round-trip finishes; other callers ignore
   * the result.
   */
  onChange?: (locale: Locale) => void | Promise<void>
}

/**
 * Shared console/marketing language persistence for the SPA pages.
 *
 * The *gesture* bookkeeping (`newtg.language.explicit` / `.pending`) belongs to
 * `I18nProvider.setLocale`; this hook only writes the server preference. A
 * failed save deliberately does **not** roll the UI back and **not** call
 * `setLocale`: the local choice stays active and `pending` survives so a later
 * launch can retry the backfill. The toast says the change is local-only.
 */
export function useLanguageSelection(signedIn: boolean): LanguageSelection {
  const { t } = useI18n()
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

  async function save(next: Locale): Promise<void> {
    // Always refetch: the endpoint's optimistic lock rejects a stale
    // `updated_at`, and the cache may be empty or invalidated by a previous
    // failed save. A rejected refetch must not escape as an unhandled
    // rejection (call sites fire the save without awaiting), so it degrades to
    // "no known server settings" and takes the local-only branch below.
    const current = await selfQuery
      .refetch()
      .then((result) => result.data)
      .catch(() => undefined)
    const updated = await persistLanguagePreference(next, current)
    if (updated === null) {
      push({ tone: 'info', message: t('common.language.saved_locally') })
      // Let the next retry pick up the fresh lock token instead of replaying
      // the stale one that just failed.
      await queryClient.invalidateQueries({ queryKey: SELF_SETTINGS_QUERY_KEY })
      return
    }
    // Write the fresh settings back so LocalePreferenceSync cannot replay the
    // old language over the one the user just picked.
    queryClient.setQueryData(SELF_SETTINGS_QUERY_KEY, updated)
    push({ tone: 'success', message: t('common.language.updated') })
  }

  return {
    onChange: signedIn ? save : undefined,
  }
}
