import { useQuery } from '@tanstack/react-query'
import { useEffect, useState, type FormEvent } from 'react'

import { isUuid, normalizeRequestId } from '@/shared/lib/request-id'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { fetchRecentFailures, fetchRequestTriage } from './api'
import { CaseFile } from './case-file'
import { FailureRail, type OwnerFilter } from './failure-rail'
import { ownerDotClass, ownerLabelKey } from './verdict-copy'

/** `<1024` 管理页闸门（UI §6.12）；无 matchMedia 的环境按桌面处理。 */
function useMinWidth(px: number): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return true
    }
    return window.matchMedia(`(min-width: ${px}px)`).matches
  })
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return
    }
    const query = window.matchMedia(`(min-width: ${px}px)`)
    const update = () => setMatches(query.matches)
    update()
    query.addEventListener('change', update)
    return () => query.removeEventListener('change', update)
  }, [px])
  return matches
}

function CaseLoading() {
  return (
    <div className="flex flex-col gap-3 px-6 py-14" aria-hidden="true" data-testid="case-loading">
      <span className="h-3 w-40 rounded bg-ink-4/30" />
      <span className="h-7 w-3/4 rounded bg-ink-4/30" />
      <span className="h-3 w-2/3 rounded bg-ink-4/30" />
    </div>
  )
}

function CaseLoadError({ onRetry }: { onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <div role="alert" className="flex items-center gap-3 px-6 py-14 text-meta">
      <span>{t('admin.failure_triage.error.case')}</span>
      <Button variant="secondary" onClick={onRetry}>
        {t('admin.failure_triage.error.retry')}
      </Button>
    </div>
  )
}

function EmptyCase() {
  const { t } = useI18n()
  return (
    <div className="max-w-[52ch] px-6 py-14 max-[760px]:px-4" data-testid="case-empty">
      <h2 className="font-serif text-stat font-semibold text-ink-1">
        {t('admin.failure_triage.empty.heading')}
      </h2>
      <p className="mt-2.5 text-meta text-ink-2">
        {t('admin.failure_triage.empty.body')}
      </p>
    </div>
  )
}

function NotFoundCase({ searched }: { searched: string }) {
  const { t } = useI18n()
  return (
    <div className="max-w-[52ch] px-6 py-14 max-[760px]:px-4" data-testid="case-not-found">
      <h2 className="font-serif text-stat font-semibold text-ink-1">
        {t('admin.failure_triage.not_found.heading')}
      </h2>
      <code className="mt-2.5 inline-block break-all font-mono text-badge text-ink-2">
        {searched}
      </code>
      <ul className="mt-3.5 list-disc pl-[18px] text-badge text-ink-2">
        <li>{t('admin.failure_triage.not_found.hint_1')}</li>
        <li>{t('admin.failure_triage.not_found.hint_2')}</li>
        <li>{t('admin.failure_triage.not_found.hint_3')}</li>
      </ul>
    </div>
  )
}

export function FailureTriagePage() {
  const { t } = useI18n()
  const wideEnough = useMinWidth(1024)
  const [owner, setOwner] = useState<OwnerFilter>('all')
  const [input, setInput] = useState('')
  const [invalid, setInvalid] = useState(false)
  const [requestId, setRequestId] = useState<string | null>(null)
  const [searchedId, setSearchedId] = useState<string | null>(null)

  const recentQuery = useQuery({
    queryKey: ['failure-triage', 'recent', owner],
    queryFn: ({ signal }) =>
      fetchRecentFailures(
        {
          range: '24h',
          limit: 20,
          ...(owner === 'all' ? {} : { owner }),
        },
        signal,
      ),
    enabled: wideEnough,
    retry: false,
  })

  const caseQuery = useQuery({
    queryKey: ['failure-triage', 'case', requestId],
    queryFn: ({ signal }) => fetchRequestTriage(requestId ?? '', signal),
    enabled: wideEnough && requestId !== null,
    retry: false,
  })

  function select(nextId: string): void {
    setInput(nextId)
    setInvalid(false)
    setRequestId(normalizeRequestId(nextId))
    setSearchedId(nextId)
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    const value = input.trim()
    if (!isUuid(value)) {
      setInvalid(true)
      return
    }
    select(normalizeRequestId(value))
  }

  if (!wideEnough) {
    return (
      <section className="rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-page-title font-semibold text-ink-1">
          {t('admin.failure_triage.title')}
        </h1>
        <p className="mt-2 text-body text-ink-2">{t('admin.failure_triage.desktop_only')}</p>
      </section>
    )
  }

  const counts = recentQuery.data?.counts
  const activeOwner = owner === 'all' ? null : owner
  /** 未加载完不渲染 0：0 会被读成「窗口内没有该责任方的失败」。 */
  const countValue = (value: number | undefined): number | string =>
    value === undefined ? '—' : value

  function renderCase() {
    if (requestId === null) return <EmptyCase />
    if (caseQuery.isPending) return <CaseLoading />
    if (caseQuery.isError || caseQuery.data === undefined) {
      return (
        <CaseLoadError
          onRetry={() => {
            void caseQuery.refetch()
          }}
        />
      )
    }
    if (!caseQuery.data.found) {
      return <NotFoundCase searched={searchedId ?? requestId} />
    }
    return (
      <CaseFile
        triage={caseQuery.data}
        onRetry={() => {
          void caseQuery.refetch()
        }}
      />
    )
  }

  return (
    <section className="flex flex-col gap-4" data-testid="failure-triage-page">
      <div className="flex flex-wrap items-end justify-between gap-7">
        <div className="min-w-[280px]">
          <h1 className="text-page-title font-semibold text-ink-1">
            {t('admin.failure_triage.title')}
          </h1>
          <p className="mt-1 max-w-[56ch] text-meta text-ink-2">
            {t('admin.failure_triage.subtitle')}
          </p>
        </div>

        <div className="flex flex-col items-end gap-2 max-[760px]:w-full max-[760px]:items-stretch">
          <form
            role="search"
            className="flex gap-2 max-[760px]:w-full"
            onSubmit={handleSubmit}
          >
            <label className="sr-only" htmlFor="failure-triage-request-id">
              {t('admin.failure_triage.search.label')}
            </label>
            <input
              id="failure-triage-request-id"
              value={input}
              onChange={(event) => {
                setInput(event.target.value)
                if (invalid) setInvalid(false)
              }}
              placeholder={t('admin.failure_triage.search.placeholder')}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={invalid}
              aria-describedby={invalid ? 'failure-triage-search-error' : undefined}
              className="h-9 w-[340px] rounded-lg border border-border-default bg-surface-card px-3 font-mono text-badge text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none max-[760px]:w-full"
            />
            <Button type="submit">{t('admin.failure_triage.search.submit')}</Button>
          </form>
          {invalid ? (
            <p
              id="failure-triage-search-error"
              role="alert"
              data-testid="search-invalid"
              className="text-badge text-danger"
            >
              {t('admin.failure_triage.search.invalid')}
            </p>
          ) : null}
          <div className="flex flex-wrap items-center gap-3.5 text-badge text-ink-2">
            <span className="text-ink-3">
              {activeOwner === null
                ? t('admin.failure_triage.totals.lead', {
                    count: countValue(counts?.total),
                  })
                : t('admin.failure_triage.totals.lead_filtered', {
                    count: countValue(counts?.total),
                    owner: t(ownerLabelKey(activeOwner)),
                  })}
            </span>
            {activeOwner !== null ? (
              <span className="inline-flex items-center gap-1.5">
                <span
                  aria-hidden="true"
                  className={['size-[7px] rounded-full', ownerDotClass(activeOwner)].join(' ')}
                />
                {t(`admin.failure_triage.totals.${activeOwner}`, {
                  count: countValue(counts?.[activeOwner]),
                })}
              </span>
            ) : (
              <>
                <span className="inline-flex items-center gap-1.5">
                  <span aria-hidden="true" className="size-[7px] rounded-full bg-verdict-user" />
                  {t('admin.failure_triage.totals.user', {
                    count: countValue(counts?.user),
                  })}
                </span>
                <span className="inline-flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className="size-[7px] rounded-full bg-verdict-channel"
                  />
                  {t('admin.failure_triage.totals.channel', {
                    count: countValue(counts?.channel),
                  })}
                </span>
                <span className="inline-flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className="size-[7px] rounded-full bg-verdict-gateway"
                  />
                  {t('admin.failure_triage.totals.gateway', {
                    count: countValue(counts?.gateway),
                  })}
                </span>
                {counts !== undefined && counts.unknown > 0 ? (
                  <span className="inline-flex items-center gap-1.5 text-ink-3">
                    <span aria-hidden="true" className="size-[7px] rounded-full bg-ink-2" />
                    {t('admin.failure_triage.totals.unknown', {
                      count: counts.unknown,
                    })}
                  </span>
                ) : null}
              </>
            )}
          </div>
        </div>
      </div>

      <div className="grid grid-cols-[296px_minmax(0,1fr)] overflow-hidden rounded-xl border border-border-hairline bg-surface-card max-[1100px]:grid-cols-1">
        <FailureRail
          data={recentQuery.data}
          isPending={recentQuery.isPending}
          isError={recentQuery.isError}
          owner={owner}
          onOwnerChange={setOwner}
          selectedId={requestId}
          onSelect={select}
          onRetry={() => {
            void recentQuery.refetch()
          }}
        />
        <section className="min-w-0">{renderCase()}</section>
      </div>
    </section>
  )
}
