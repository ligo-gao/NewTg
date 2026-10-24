import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { usePublicStatus } from '@/lib/public-status'
import { TurnstileWidget } from '@/features/auth/TurnstileWidget'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { fetchCheckinCalendar, submitCheckin } from './api'
import { formatCreditsValue } from './format'

export interface CheckinCardProps {
  /**
   * Replaces the standalone `mt-4` when the card is laid out inside a grid
   * that already owns the block spacing (pass `''` there).
   */
  className?: string
}

/**
 * Dashboard check-in card (modules-01 §3.19 F-01-30).
 *
 * Visibility is driven by the derived `checkin_enabled` flag on
 * `GET /api/status` — the operator's `checkin.*` settings never reach the
 * public contract. When the flag is false the card renders nothing, so a
 * disabled site neither downloads a calendar nor shows a dead button.
 *
 * The server owns the clock (site timezone) and the reward draw. This card
 * treats a 409 `already_checked_in` as a successful replay: it refreshes the
 * calendar and shows the stored day instead of inventing a second award.
 * Turnstile is the fifth verification point, so when it is enabled the button
 * stays disabled until a fresh (single-use) token arrives.
 */
export function CheckinCard({ className = 'mt-4' }: CheckinCardProps = {}) {
  const { t, locale } = useI18n()
  const status = usePublicStatus()
  const queryClient = useQueryClient()

  const enabled = status.data?.checkin_enabled === true
  const turnstileEnabled = status.data?.auth.turnstile.enabled === true
  const turnstileSiteKey = status.data?.auth.turnstile.site_key ?? ''

  const [turnstileToken, setTurnstileToken] = useState<string | undefined>(undefined)
  const [turnstileReset, setTurnstileReset] = useState(0)
  const [feedback, setFeedback] = useState<'success' | 'already' | undefined>(undefined)
  const [awarded, setAwarded] = useState<number | undefined>(undefined)

  const calendar = useQuery({
    queryKey: ['checkin', 'calendar'] as const,
    queryFn: fetchCheckinCalendar,
    enabled,
    staleTime: 0,
  })

  const mutation = useMutation({
    mutationFn: () => submitCheckin(turnstileToken),
    onSuccess: (result) => {
      setFeedback('success')
      setAwarded(result.awarded)
      setTurnstileToken(undefined)
      setTurnstileReset((value) => value + 1)
      void queryClient.invalidateQueries({ queryKey: ['checkin', 'calendar'] })
    },
    onError: (error) => {
      // A same-day replay is not a failure: the first request already landed.
      // Refresh so the card shows the persisted award rather than a stale
      // "not checked in" state.
      if (error instanceof ApiError && error.type.endsWith('/already_checked_in')) {
        setFeedback('already')
        setAwarded(undefined)
        void queryClient.invalidateQueries({ queryKey: ['checkin', 'calendar'] })
      }
      // Single-use tokens: always reset after an attempt, success or failure.
      setTurnstileToken(undefined)
      setTurnstileReset((value) => value + 1)
    },
  })

  const data = calendar.data

  const grid = useMemo(() => {
    if (data === undefined) return undefined
    const [yearText, monthText] = data.month.split('-')
    const year = Number(yearText)
    const month = Number(monthText)
    if (!Number.isFinite(year) || !Number.isFinite(month)) return undefined
    const first = new Date(Date.UTC(year, month - 1, 1))
    const daysInMonth = new Date(Date.UTC(year, month, 0)).getUTCDate()
    // Monday-first grid (design keeps seven columns at every breakpoint).
    const leading = (first.getUTCDay() + 6) % 7
    const awardedByDate = new Map(data.days.map((day) => [day.date, day.awarded]))
    const cells: Array<{ date?: string; day?: number; awarded?: number }> = []
    for (let index = 0; index < leading; index += 1) cells.push({})
    for (let day = 1; day <= daysInMonth; day += 1) {
      const date = `${data.month}-${String(day).padStart(2, '0')}`
      cells.push({ date, day, awarded: awardedByDate.get(date) })
    }
    // Pad the tail so every ARIA grid row has seven cells; padding is
    // presentational only (aria-hidden) and never carries a date.
    while (cells.length % 7 !== 0) cells.push({})
    const weeks: Array<typeof cells> = []
    for (let index = 0; index < cells.length; index += 7) {
      weeks.push(cells.slice(index, index + 7))
    }
    return { weeks, daysInMonth }
  }, [data])

  const weekdayLabels = useMemo(() => {
    // timeZone: 'UTC' is required: the reference dates below are built with
    // Date.UTC and the grid is Monday-first. Without it a viewer in a negative
    // offset (e.g. America/New_York) sees Sunday-first labels shifted by a day.
    const formatter = new Intl.DateTimeFormat(locale, {
      weekday: 'narrow',
      timeZone: 'UTC',
    })
    // 2024-01-01 is a Monday; stable reference for Monday-first labels.
    return Array.from({ length: 7 }, (_, index) =>
      formatter.format(new Date(Date.UTC(2024, 0, 1 + index))),
    )
  }, [locale])

  // The status probe is the gate: while unknown/disabled the card stays out.
  if (!enabled) return null

  const checkedToday = data?.checked_in_today === true
  const todayAward =
    data === undefined
      ? undefined
      : data.days.find((day) => day.date === data.today)?.awarded
  const offline =
    (calendar.error instanceof ApiError && calendar.error.kind === 'offline') ||
    (mutation.error instanceof ApiError && mutation.error.kind === 'offline')
  const mutationError =
    mutation.error !== undefined &&
    !(mutation.error instanceof ApiError && mutation.error.type.endsWith('/already_checked_in'))
      ? mutation.error
      : undefined

  return (
    <section
      data-testid="checkin-card"
      aria-labelledby="checkin-title"
      className={[
        'rounded-xl border border-border-default bg-surface-card p-4',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 id="checkin-title" className="text-card-title font-semibold text-ink-1">
            {t('common.dashboard.checkin.title')}
          </h3>
          <p className="mt-1 text-meta text-ink-2">
            {t('common.dashboard.checkin.description')}
          </p>
        </div>
        <Button
          data-testid="checkin-button"
          type="button"
          className="h-9"
          disabled={
            checkedToday ||
            mutation.isPending ||
            calendar.isPending ||
            (turnstileEnabled && turnstileToken === undefined)
          }
          onClick={() => {
            setFeedback(undefined)
            mutation.mutate()
          }}
        >
          {checkedToday
            ? t('common.dashboard.checkin.checked_button')
            : mutation.isPending
              ? t('common.dashboard.checkin.submitting')
              : t('common.dashboard.checkin.button')}
        </Button>
      </div>

      {turnstileEnabled && !checkedToday ? (
        <div className="mt-3">
          <TurnstileWidget
            siteKey={turnstileSiteKey}
            onToken={setTurnstileToken}
            resetSignal={turnstileReset}
          />
        </div>
      ) : null}

      {feedback !== undefined ? (
        <p
          data-testid="checkin-feedback"
          role="status"
          className="mt-3 text-body text-ink-1"
        >
          {feedback === 'success'
            ? t('common.dashboard.checkin.success', {
                amount: formatCreditsValue(awarded ?? 0, locale),
              })
            : t('common.dashboard.checkin.already')}
        </p>
      ) : null}

      {offline ? (
        <p data-testid="checkin-offline" role="alert" className="mt-3 text-body text-danger">
          {t('common.dashboard.checkin.offline')}
        </p>
      ) : null}

      {mutationError !== undefined && !offline ? (
        <p data-testid="checkin-mutation-error" role="alert" className="mt-3 text-body text-danger">
          {t('common.dashboard.checkin.action_error')}
        </p>
      ) : null}

      {calendar.isPending ? (
        <div
          data-testid="checkin-skeleton"
          className="mt-3 h-28 animate-pulse rounded-lg bg-surface-hover motion-reduce:animate-none"
        />
      ) : calendar.isError ? (
        <div className="mt-3 flex items-center gap-3">
          <p data-testid="checkin-error" className="text-body text-ink-2">
            {t('common.dashboard.checkin.error')}
          </p>
          <button
            type="button"
            data-testid="checkin-retry"
            onClick={() => void calendar.refetch()}
            className="rounded border border-border-default px-2 py-0.5 text-badge text-ink-2 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('common.action.retry')}
          </button>
        </div>
      ) : data !== undefined && grid !== undefined ? (
        <>
          <dl className="mt-3 grid grid-cols-1 gap-2 text-meta text-ink-2 sm:grid-cols-3">
            <div>
              <dt>{t('common.dashboard.checkin.streak_label')}</dt>
              <dd data-testid="checkin-streak" className="text-ink-1">
                {t('common.dashboard.checkin.streak', { days: data.streak })}
              </dd>
            </div>
            <div>
              <dt>{t('common.dashboard.checkin.month_total_label')}</dt>
              <dd data-testid="checkin-month-total" className="text-ink-1">
                {formatCreditsValue(data.month_total, locale)}
              </dd>
            </div>
            <div>
              <dt>{t('common.dashboard.checkin.total_earned_label')}</dt>
              <dd data-testid="checkin-total-earned" className="text-ink-1">
                {formatCreditsValue(data.total_earned, locale)}
              </dd>
            </div>
          </dl>

          {data.days.length === 0 ? (
            <p data-testid="checkin-empty" className="mt-3 text-meta text-ink-2">
              {t('common.dashboard.checkin.empty')}
            </p>
          ) : null}

          <div
            data-testid="checkin-calendar"
            role="grid"
            aria-label={t('common.dashboard.checkin.calendar_aria', {
              month: data.month,
            })}
            className="mt-3"
          >
            <div role="row" className="grid grid-cols-7 gap-1">
              {weekdayLabels.map((label, index) => (
                <div
                  key={`${label}-${index}`}
                  role="columnheader"
                  className="text-center text-badge text-ink-3"
                >
                  {label}
                </div>
              ))}
            </div>
            {grid.weeks.map((week, weekIndex) => (
              <div
                role="row"
                key={`week-${weekIndex}`}
                className="mt-1 grid grid-cols-7 gap-1"
              >
                {week.map((cell, index) =>
                  cell.day === undefined ? (
                    <div
                      key={`blank-${weekIndex}-${index}`}
                      role="gridcell"
                      aria-hidden="true"
                    />
                  ) : (
                    <div
                      key={cell.date ?? `day-${weekIndex}-${index}`}
                      role="gridcell"
                      data-testid={`checkin-day-${cell.date}`}
                      aria-label={
                        cell.awarded === undefined
                          ? cell.date
                          : t('common.dashboard.checkin.day_awarded', {
                              date: cell.date ?? '',
                              amount: formatCreditsValue(cell.awarded, locale),
                            })
                      }
                      className={[
                        'flex h-8 items-center justify-center rounded text-badge',
                        cell.awarded === undefined
                          ? 'text-ink-3'
                          : 'bg-surface-hover font-semibold text-ink-1',
                        cell.date === data.today
                          ? 'border border-ink-3'
                          : '',
                      ].join(' ')}
                    >
                      {cell.day}
                    </div>
                  ),
                )}
              </div>
            ))}
          </div>

          {todayAward !== undefined ? (
            <p data-testid="checkin-today-award" className="mt-2 text-meta text-ink-2">
              {t('common.dashboard.checkin.today_award', {
                amount: formatCreditsValue(todayAward, locale),
              })}
            </p>
          ) : null}
        </>
      ) : null}
    </section>
  )
}
