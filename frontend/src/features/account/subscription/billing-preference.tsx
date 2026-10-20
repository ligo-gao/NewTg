import { useEffect, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useQueryClient, type UseQueryResult } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import type { BillingPreference, BillingPreferenceValue } from './types'

export const BILLING_PREFERENCE_KEY = ['subscription', 'preference'] as const

const VALUES: readonly BillingPreferenceValue[] = [
  'subscription_first',
  'wallet_first',
  'subscription_only',
  'wallet_only',
]

export interface BillingPreferenceCardProps {
  query: UseQueryResult<BillingPreference>
  /** Writes are disabled while offline. */
  offline: boolean
}

/**
 * 扣费偏好（billing-08 §4.2 / §16.3 F-H-04）。
 *
 * Four frozen literals, saved immediately on click. The control is optimistic:
 * the radio moves at once, and a failed `PUT` rolls it back to the previously
 * saved value and re-reads the server state — showing the new value after a
 * 5xx would misrepresent which balance will pay. An unknown/empty server value
 * has already been normalized to `subscription_first` by the API, so this card
 * never needs a client-side default of its own.
 */
export function BillingPreferenceCard({
  query,
  offline,
}: BillingPreferenceCardProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [selected, setSelected] = useState<BillingPreferenceValue | null>(null)
  const [pending, setPending] = useState(false)

  const serverValue = query.data?.preference ?? null

  // Adopt the server value whenever it changes, except while a save is in
  // flight: the in-flight optimistic selection must not be overwritten by the
  // still-old cached value.
  useEffect(() => {
    if (!pending) setSelected(serverValue)
  }, [serverValue, pending])

  async function save(next: BillingPreferenceValue): Promise<void> {
    if (pending || offline || selected === next) return
    const previous = selected
    setSelected(next)
    setPending(true)
    try {
      const updated = requireData(
        await apiClient.PUT('/api/billing-preference', {
          body: { preference: next },
        }),
      )
      // Write the server's normalized value into the shared cache instead of
      // refetching: a successful save must never be undone by a failed refetch
      // that leaves the stale value in place.
      queryClient.setQueryData(BILLING_PREFERENCE_KEY, updated)
      setSelected(updated.preference)
      push({
        tone: 'success',
        message: t('account.subscription.toast.preference_saved'),
      })
    } catch {
      setSelected(previous)
      push({
        tone: 'error',
        message: t('account.subscription.preference.save_failed'),
      })
      // Re-read the authoritative value: the rollback restores what the user
      // saw, and the refetch catches any concurrent change made elsewhere.
      queryClient
        .invalidateQueries({ queryKey: BILLING_PREFERENCE_KEY })
        .catch(() => undefined)
    } finally {
      setPending(false)
    }
  }

  function moveFocus(event: ReactKeyboardEvent<HTMLDivElement>): void {
    const directions: Record<string, number> = {
      ArrowRight: 1,
      ArrowDown: 1,
      ArrowLeft: -1,
      ArrowUp: -1,
    }
    const step = directions[event.key]
    if (step === undefined) return
    const radios = Array.from(
      event.currentTarget.querySelectorAll<HTMLElement>('[role="radio"]'),
    )
    const index = radios.indexOf(event.target as HTMLElement)
    if (index === -1) return
    event.preventDefault()
    const next = radios[(index + step + radios.length) % radios.length]
    next?.focus()
    next?.click()
  }

  return (
    <section
      aria-labelledby="subscription-preference-title"
      className="rounded-xl border border-border-hairline bg-surface-card p-5"
    >
      <h2
        id="subscription-preference-title"
        className="text-[15px] font-semibold text-ink-1"
      >
        {t('account.subscription.preference.label')}
      </h2>
      <p className="mt-1 text-[13px] text-ink-2">
        {t('account.subscription.preference.description')}
      </p>

      {query.isPending ? (
        <div
          role="status"
          aria-label={t('account.subscription.preference.loading')}
          aria-busy="true"
          className="mt-4 grid gap-2 sm:grid-cols-2"
        >
          <div aria-hidden="true" className="h-16 animate-pulse rounded-lg bg-surface-subtle" />
          <div aria-hidden="true" className="h-16 animate-pulse rounded-lg bg-surface-subtle" />
        </div>
      ) : query.isError ? (
        <div className="mt-4 flex items-center gap-3 text-sm text-ink-2">
          <span>{t('account.subscription.preference.load_error')}</span>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('account.subscription.mine.retry')}
          </Button>
        </div>
      ) : (
        <div
          role="radiogroup"
          aria-label={t('account.subscription.preference.label')}
          className="mt-4 grid gap-2 sm:grid-cols-2"
          onKeyDown={moveFocus}
        >
          {VALUES.map((value) => {
            const checked = selected === value
            return (
              <button
                key={value}
                type="button"
                role="radio"
                aria-checked={checked}
                disabled={offline || pending}
                onClick={() => void save(value)}
                className={[
                  'rounded-lg border p-3 text-left transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60',
                  checked
                    ? 'border-[1.5px] border-brand bg-brand-subtle'
                    : 'border-border-default bg-surface-page hover:bg-surface-hover',
                ].join(' ')}
              >
                <span className="flex items-center gap-2">
                  <span
                    aria-hidden="true"
                    className={[
                      'inline-flex size-4 items-center justify-center rounded-full border',
                      checked ? 'border-brand' : 'border-border-default',
                    ].join(' ')}
                  >
                    {checked ? (
                      <span className="size-2 rounded-full bg-brand" />
                    ) : null}
                  </span>
                  <span className="text-sm font-medium text-ink-1">
                    {t(`account.subscription.preference.${value}`)}
                  </span>
                </span>
                <span className="mt-1 block pl-6 text-xs text-ink-2">
                  {t(`account.subscription.preference.${value}_desc`)}
                </span>
              </button>
            )
          })}
        </div>
      )}
    </section>
  )
}
