import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import {
  CSRF_HEADER,
  getCsrfToken,
  isCsrfFailure,
  refreshCsrfCookie,
  toApiError,
} from '@/api/client'
import { ApiError, parseProblem } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'

import { SystemCard } from './system-card'

/**
 * F-08-42 更新检查卡（modules-08 §3.15、UI §6.7）。
 *
 * 关键边界：
 *  - 本卡不参与页面 10s 轮询：缓存由服务端 TTL 管理，只有「立即检查」或
 *    缓存过期后的首次读取才触发出站。
 *  - 服务端响应不含 source_url；前端也绝不拼接/展示更新源地址。
 *  - `notes` 按纯文本渲染；`notes_url` 只作外链（rel=noopener noreferrer）。
 */
export type UpdateCheckState =
  | 'disabled'
  | 'source_unset'
  | 'up_to_date'
  | 'update_available'
  | 'incomparable'
  | 'error'

export type UpdateErrorKind =
  | 'network'
  | 'timeout'
  | 'rate_limit'
  | 'payload'
  | 'ssrf_blocked'
  | 'in_progress'

export interface UpdateCheckSuccess {
  latest: string
  checked_at: string
  published_at?: string
  notes_url?: string
  prerelease?: boolean
  notes?: string
}

export interface UpdateCheckResult {
  enabled: boolean
  source_configured: boolean
  channel: 'stable' | 'beta'
  current_version: string
  state: UpdateCheckState
  cached: boolean
  cache_state: 'fresh' | 'stale' | 'miss'
  checked_at?: string
  latest?: string
  published_at?: string
  notes_url?: string
  prerelease?: boolean
  notes?: string
  prerelease_skipped?: boolean
  error?: string
  error_kind?: UpdateErrorKind
  last_success?: UpdateCheckSuccess
  min_check_interval_seconds: number
}

function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

async function dispatch(init: RequestInit): Promise<Response> {
  try {
    return await fetch(apiUrl('/api/system/update-check'), init)
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
}

export function fetchUpdateCheck(signal?: AbortSignal): Promise<UpdateCheckResult> {
  return dispatch({
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  }).then(async (response) => {
    if (!response.ok) throw await toApiError(response)
    return (await response.json()) as UpdateCheckResult
  })
}

export async function triggerUpdateCheck(): Promise<UpdateCheckResult> {
  const headers = new Headers({ Accept: 'application/json' })
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)
  const init: RequestInit = {
    method: 'POST',
    credentials: 'same-origin',
    headers,
  }

  let response = await dispatch(init)
  if (!response.ok && (await isCsrfFailure(response))) {
    await refreshCsrfCookie()
    headers.delete(CSRF_HEADER)
    const refreshed = getCsrfToken()
    if (refreshed !== undefined) headers.set(CSRF_HEADER, refreshed)
    response = await dispatch(init)
  }
  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as UpdateCheckResult
}

function formatTimestamp(value: string | undefined, locale: string): string {
  if (!value) return '—'
  const timestamp = Date.parse(value)
  return Number.isNaN(timestamp) ? value : new Date(timestamp).toLocaleString(locale)
}

function errorKey(kind: UpdateErrorKind | undefined): string {
  switch (kind) {
    case 'network':
    case 'timeout':
    case 'rate_limit':
    case 'payload':
    case 'in_progress':
      return `settings.status.update_error_${kind}`
    case 'ssrf_blocked':
      return 'settings.status.update_error_ssrf'
    default:
      return 'settings.status.update_error'
  }
}

export function SystemUpdateCard({ currentVersion }: { currentVersion?: string }) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const countdown = useCountdown()

  const query = useQuery({
    queryKey: ['system-update-check'],
    queryFn: ({ signal }) => fetchUpdateCheck(signal),
    // 页面按需：不参与 10s 轮询；服务端 TTL 决定何时重新抓取。
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
    staleTime: 60_000,
  })

  const mutation = useMutation({
    mutationFn: triggerUpdateCheck,
    onMutate: () => countdown.stop(),
    onSuccess: (result) => {
      queryClient.setQueryData(['system-update-check'], result)
    },
    onError: (error) => {
      if (error instanceof ApiError && error.status === 429) {
        // 服务端 Retry-After 优先；缺失时退回文档冻结的 60s。
        countdown.start(error.retry_after ?? 60)
      }
    },
  })

  const result = mutation.data ?? query.data
  const busy = mutation.isPending || countdown.running
  const current = result?.current_version || currentVersion || '—'

  let message = ''
  if (result) {
    switch (result.state) {
      case 'disabled':
        message = t('settings.status.update_disabled')
        break
      case 'source_unset':
        message = t('settings.status.update_source_unset')
        break
      case 'up_to_date':
        message = t('settings.status.update_up_to_date')
        break
      case 'update_available':
        message = t('settings.status.update_available', { version: result.latest ?? '—' })
        break
      case 'incomparable':
        message = t('settings.status.update_incomparable')
        break
      case 'error':
        message = t(errorKey(result.error_kind))
        break
    }
  }

  return (
    <SystemCard
      title={t('settings.status.update_title')}
      testId="system-status-update"
      action={
        <Button
          variant="secondary"
          onClick={() => mutation.mutate()}
          disabled={busy || !result?.enabled || !result?.source_configured}
          data-testid="system-update-check-now"
        >
          {mutation.isPending
            ? t('settings.status.update_checking')
            : t('settings.status.update_check_now')}
        </Button>
      }
    >
      <p className="text-xs text-ink-3">{t('settings.status.update_no_auto_refresh')}</p>

      <dl className="mt-3 grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
        <div>
          <dt className="text-xs text-ink-3">{t('settings.status.update_current')}</dt>
          <dd className="font-mono text-ink-1">{current}</dd>
        </div>
        {result?.latest ? (
          <div>
            <dt className="text-xs text-ink-3">{t('settings.status.update_latest')}</dt>
            <dd className="flex flex-wrap items-center gap-2 font-mono text-ink-1">
              <span data-testid="system-update-latest">{result.latest}</span>
              {result.prerelease ? (
                <StatusBadge variant="warning">
                  {t('settings.status.update_prerelease')}
                </StatusBadge>
              ) : null}
            </dd>
          </div>
        ) : null}
      </dl>

      {query.isPending && !result ? (
        <p className="mt-3 text-sm text-ink-2" role="status">
          {t('settings.status.update_loading')}
        </p>
      ) : null}

      {query.isError && !result ? (
        <div className="mt-3 space-y-3">
          <FormAlert tone="danger">
            {t('settings.status.update_error')}{' '}
            {query.error instanceof ApiError ? (query.error.detail ?? query.error.title) : ''}
          </FormAlert>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('settings.status.update_retry')}
          </Button>
        </div>
      ) : null}

      {result ? (
        <div className="mt-3 space-y-2">
          <p className="flex items-center gap-2 text-sm text-ink-1">
            <span data-testid="system-update-state">{message}</span>
            {result.cached ? (
              <StatusBadge variant="neutral">{t('settings.status.update_cached')}</StatusBadge>
            ) : null}
          </p>

          {result.state === 'incomparable' ? (
            <p className="text-xs text-ink-3">{t('settings.status.update_not_injected')}</p>
          ) : null}

          {result.prerelease_skipped ? (
            <p className="text-xs text-ink-3">{t('settings.status.update_prerelease_skipped')}</p>
          ) : null}

          {result.state === 'error' ? (
            <p className="text-sm text-danger" data-testid="system-update-error">
              {t(errorKey(result.error_kind))}
            </p>
          ) : null}

          {result.published_at ? (
            <p className="text-xs text-ink-3">
              {t('settings.status.update_published_at', {
                time: formatTimestamp(result.published_at, locale),
              })}
            </p>
          ) : null}
          {result.checked_at ? (
            <p className="text-xs text-ink-3">
              {t('settings.status.update_checked_at', {
                time: formatTimestamp(result.checked_at, locale),
              })}
            </p>
          ) : null}

          {result.last_success && result.state === 'error' ? (
            <p className="text-xs text-ink-3">
              {t('settings.status.update_last_success', {
                version: result.last_success.latest,
                time: formatTimestamp(result.last_success.checked_at, locale),
              })}
            </p>
          ) : null}

          {result.notes ? (
            <div className="text-sm text-ink-2">
              <p className="text-xs text-ink-3">{t('settings.status.update_notes')}</p>
              <p className="mt-1 whitespace-pre-wrap break-words font-mono text-xs">
                {result.notes}
              </p>
            </div>
          ) : null}

          {result.notes_url ? (
            <a
              href={result.notes_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm text-brand underline-offset-2 hover:underline"
            >
              {t('settings.status.update_view_notes')}
            </a>
          ) : null}

          {countdown.running ? (
            <p className="text-xs text-ink-3" role="status">
              {t('settings.status.update_retry_in', { seconds: countdown.seconds })}
            </p>
          ) : null}

          {result.state === 'disabled' ? (
            <p className="text-xs text-ink-3">{t('settings.status.update_disabled_hint')}</p>
          ) : null}
          {result.state === 'source_unset' ? (
            <p className="text-xs text-ink-3">{t('settings.status.update_source_unset_hint')}</p>
          ) : null}
        </div>
      ) : null}
    </SystemCard>
  )
}
