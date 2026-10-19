import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits, formatTokenAmount } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import type { LogsCapabilities } from './capabilities'
import { formatPercent, formatSeconds, formatUnitPrice } from './format'
import { UsageStatusBadge } from './log-status-badge'
import type { UsageLogDetail } from './search'

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)
    : undefined
}

function readString(
  source: Record<string, unknown> | undefined,
  keys: readonly string[],
): string | undefined {
  for (const key of keys) {
    const value = source?.[key]
    if (typeof value === 'string' && value !== '') return value
  }
  return undefined
}

function readNumber(
  source: Record<string, unknown> | undefined,
  keys: readonly string[],
): number | undefined {
  for (const key of keys) {
    const value = source?.[key]
    if (typeof value === 'number' && Number.isFinite(value)) return value
  }
  return undefined
}

/**
 * Prompt-like fields are only ever rendered masked. The detail contract has no
 * prompt carrier at all (the prompt endpoint answers 404 for every row in this
 * release); if a future carrier adds one, the UI must never echo plaintext.
 */
function readPromptField(detail: UsageLogDetail): string | undefined {
  const record = detail as unknown as Record<string, unknown>
  const direct = readString(record, ['prompt', 'prompt_text', 'prompt_preview'])
  if (direct !== undefined) return direct
  return readString(asRecord(detail.other), [
    'prompt',
    'prompt_text',
    'prompt_preview',
  ])
}

function Section({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  return (
    <section className="border-t border-border-hairline pt-4 first:border-0 first:pt-0">
      <h3 className="mb-3 flex items-center gap-2 text-[16px] font-semibold leading-6 text-ink-1">
        <span aria-hidden="true" className="h-4 w-[3px] rounded bg-brand" />
        {title}
      </h3>
      {children}
    </section>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-[13px] text-ink-3">{label}</dt>
      <dd className="min-w-0 break-words text-sm text-ink-1">{children}</dd>
    </div>
  )
}

function SkeletonSection() {
  return (
    <div className="flex flex-col gap-2" aria-hidden="true">
      <span className="h-3 w-32 rounded bg-ink-4/30" />
      <span className="h-3 w-3/4 rounded bg-ink-4/30" />
      <span className="h-3 w-2/3 rounded bg-ink-4/30" />
    </div>
  )
}

export interface LogDetailDrawerProps {
  logId: number | null
  onOpenChange: (open: boolean) => void
  capabilities: LogsCapabilities
  /**
   * F-05-06 会话内开关。false 时请求缺省打码，并且 UI 再做一层客户端打码，
   * 避免把「服务端应已打码」当成渲染前提。
   */
  revealSensitive: boolean
}

/**
 * F-05-02 detail drawer. Sections A–D follow sub-feature §4.4; the retry chain
 * is only rendered when the row carries one. Billing rows are derived from the
 * token quantities and the unit-price snapshot if the backend exposes it —
 * nothing is recomputed for the settled/refunded totals, which are frozen
 * ledger snapshots (module 03).
 */
export function LogDetailDrawer({
  logId,
  onOpenChange,
  capabilities,
  revealSensitive,
}: LogDetailDrawerProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const [promptRequesting, setPromptRequesting] = useState(false)

  const detailQuery = useQuery({
    queryKey: ['usage-logs', 'detail', logId, revealSensitive],
    enabled: logId !== null,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-logs/{id}', {
          params: {
            path: { id: logId as number },
            // OpenAPI 冻结稿尚未收录 reveal_sensitive；后端已实现并按
            // log.prompt.view 重校验，因此这里显式带上会话开关。
            ...(revealSensitive
              ? { query: { reveal_sensitive: 1 } }
              : {}),
          } as never,
        }),
      ),
  })

  const detail = detailQuery.data
  const other = asRecord(detail?.other)
  const notFound =
    detailQuery.error instanceof ApiError && detailQuery.error.kind === 'not_found'

  function copy(value: string): void {
    void navigator.clipboard?.writeText(value).catch(() => undefined)
    push({ tone: 'success', message: t('common.action.copied') })
  }

  async function openPrompt(): Promise<void> {
    if (logId === null || promptRequesting) return
    setPromptRequesting(true)
    try {
      await apiClient.GET('/api/usage-logs/{id}/prompt', {
        params: { path: { id: logId } },
      })
    } catch {
      // No prompt carrier in this release: the endpoint 404s for every row.
      push({ tone: 'info', message: t('logs.usage.detail.prompt_unavailable') })
    } finally {
      setPromptRequesting(false)
    }
  }

  const promptField = detail === undefined ? undefined : readPromptField(detail)
  const retryChain = detail?.retry_chain ?? []

  const totalTokens =
    (detail?.prompt_tokens ?? 0) +
    (detail?.completion_tokens ?? 0) +
    (detail?.cache_read_tokens ?? 0) +
    (detail?.cache_write_tokens ?? 0)

  const unitPrices = asRecord(other?.unit_prices)
  const inputPrice = readNumber(unitPrices, ['input', 'prompt'])
  const outputPrice = readNumber(unitPrices, ['output', 'completion'])
  const cacheReadPrice = readNumber(unitPrices, ['cache_read'])
  const cacheWritePrice = readNumber(unitPrices, ['cache_write'])
  const groupRatio = readNumber(other, ['group_ratio'])
  const estimated = other?.estimated === true

  const billingRows = [
    {
      key: 'input_cost' as const,
      quantity: detail?.prompt_tokens ?? 0,
      price: inputPrice,
      always: true,
    },
    {
      key: 'output_cost' as const,
      quantity: detail?.completion_tokens ?? 0,
      price: outputPrice,
      always: true,
    },
    {
      key: 'cache_read_cost' as const,
      quantity: detail?.cache_read_tokens ?? 0,
      price: cacheReadPrice,
      always: false,
    },
    {
      key: 'cache_write_cost' as const,
      quantity: detail?.cache_write_tokens ?? 0,
      price: cacheWritePrice,
      always: false,
    },
  ].filter((row) => row.always || row.quantity > 0)

  const attempts = retryChain.length > 0 ? retryChain.length : 1
  // `retry_chain` contains every attempt, so 3 steps = 2 retries.
  const retries = Math.max(0, retryChain.length - 1)
  const lastRetry = asRecord(retryChain.at(-1))
  const lastError = readString(lastRetry, ['error_class', 'error'])
  const failureClass =
    detail?.error_class ?? (lastError ?? t('logs.usage.status.error'))

  function conclusion(): string | null {
    if (detail === undefined || detail.status === 'refused') return null
    const pre = formatCredits(detail.pre_deducted ?? 0, locale)
    if (detail.status === 'error') {
      return t('logs.usage.detail.conclusion_failed', { pre })
    }
    if (detail.refunded > 0) {
      return t('logs.usage.detail.conclusion_success', {
        pre,
        cost: formatCredits(detail.cost, locale),
        refunded: formatCredits(detail.refunded, locale),
      })
    }
    return null
  }

  const conclusionText = conclusion()

  return (
    <Dialog
      open={logId !== null}
      onOpenChange={onOpenChange}
      title={detail?.model ?? t('logs.usage.detail.title')}
      variant="sheet"
      panelClassName="max-w-[640px]"
      footer={
        <Button variant="secondary" onClick={() => onOpenChange(false)}>
          {t('logs.usage.detail.close')}
        </Button>
      }
    >
      {detailQuery.isPending ? (
        <div className="flex flex-col gap-5" data-testid="detail-skeleton">
          <SkeletonSection />
          <SkeletonSection />
          <SkeletonSection />
          <SkeletonSection />
        </div>
      ) : notFound ? (
        <p className="py-8 text-center text-sm text-ink-2">
          {t('logs.usage.detail.not_found')}
        </p>
      ) : detailQuery.isError || detail === undefined ? (
        <div role="alert" className="flex items-center gap-3 py-6 text-sm">
          <span>{t('logs.usage.error.load')}</span>
          <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
            {t('logs.usage.error.retry')}
          </Button>
        </div>
      ) : (
        <div className="flex flex-col gap-5">
          <div className="flex flex-wrap items-center gap-3">
            <UsageStatusBadge status={detail.status} />
            <span
              className="text-[13px] text-ink-2"
              title={new Date(detail.created_at).toISOString()}
            >
              {formatRelativeTime(detail.created_at, locale)}
            </span>
            <Button
              variant="ghost"
              className="h-7 px-2"
              aria-label={t('logs.usage.copy.request_id')}
              onClick={() => {
                copy(detail.request_id)
              }}
            >
              {t('logs.usage.copy.request_id')}
            </Button>
          </div>

          {/* A. General */}
          <Section title={t('logs.usage.detail.general')}>
            <dl className="grid grid-cols-[150px_1fr] gap-x-4 gap-y-2">
              <Field label={t('logs.usage.detail.request_id')}>
                <span className="font-mono text-[13px]">{detail.request_id}</span>
              </Field>
              <Field label={t('logs.usage.detail.time')}>
                <span title={new Date(detail.created_at).toISOString()}>
                  {new Date(detail.created_at).toLocaleString(locale)}
                </span>
              </Field>
              <Field label={t('logs.usage.detail.model_group')}>
                <span className="font-mono text-[13px]">{detail.model}</span>
                {' / '}
                {detail.group ?? '—'}
              </Field>
              <Field label={t('logs.usage.detail.token_channel')}>
                {detail.token_name ?? '—'}
                {' / '}
                {detail.upstream ?? '—'}
                {capabilities.isAdmin && detail.channel_id !== null &&
                detail.channel_id !== undefined ? (
                  <span className="ml-2 text-[12px] text-ink-3">
                    {t('logs.usage.detail.channel_id', { id: detail.channel_id })}
                  </span>
                ) : null}
              </Field>
              <Field label={t('logs.usage.detail.stream')}>
                {detail.stream
                  ? t('logs.usage.detail.stream_yes')
                  : t('logs.usage.detail.stream_no')}
              </Field>
            </dl>
          </Section>

          {/* B. Usage parameters */}
          <Section title={t('logs.usage.detail.usage')}>
            <dl className="grid grid-cols-1 gap-x-6 gap-y-2 sm:grid-cols-2">
              <Field label={t('logs.usage.detail.input_tokens')}>
                <TokenStat value={detail.prompt_tokens} total={totalTokens} />
              </Field>
              <Field label={t('logs.usage.detail.output_tokens')}>
                <TokenStat value={detail.completion_tokens} total={totalTokens} />
              </Field>
              <Field label={t('logs.usage.detail.cache_read_tokens')}>
                <TokenStat value={detail.cache_read_tokens} total={totalTokens} />
              </Field>
              <Field label={t('logs.usage.detail.cache_write_tokens')}>
                <TokenStat value={detail.cache_write_tokens} total={totalTokens} />
              </Field>
            </dl>
            {estimated ? (
              <p className="mt-2 text-xs text-warning">
                {t('logs.usage.detail.estimated')}
              </p>
            ) : null}
          </Section>

          {/* C. Billing details (absent for refused rows — no funds moved). */}
          {detail.status !== 'refused' ? (
            <Section title={t('logs.usage.detail.billing')}>
              <dl className="flex flex-col gap-2">
                {billingRows.map((row) => (
                  <div
                    key={row.key}
                    className="flex flex-wrap items-baseline justify-between gap-2 font-mono text-[13px] tabular-nums"
                  >
                    <dt className="text-ink-2">
                      {t(`logs.usage.detail.${row.key}`)}
                    </dt>
                    <dd className="text-ink-1">
                      {formatTokenAmount(row.quantity, locale)}
                      {row.price !== undefined ? (
                        <>
                          {' × '}
                          {t('logs.usage.detail.per_million', {
                            price: formatUnitPrice(row.price),
                          })}
                          {' = '}
                          {formatCredits(
                            Math.round((row.quantity * row.price) / 1_000_000),
                            locale,
                          )}
                        </>
                      ) : null}
                    </dd>
                  </div>
                ))}
                {groupRatio !== undefined ? (
                  <div className="flex items-baseline justify-between gap-2 font-mono text-[13px] tabular-nums">
                    <dt className="text-ink-2">
                      {t('logs.usage.detail.group_ratio')}
                    </dt>
                    <dd className="text-ink-1">
                      {detail.group ?? '—'} × {groupRatio}
                    </dd>
                  </div>
                ) : null}
              </dl>
              <dl className="mt-3 flex flex-col gap-2 border-t border-border-hairline pt-3 text-sm tabular-nums">
                <div className="flex justify-between gap-2">
                  <dt className="text-ink-2">
                    {t('logs.usage.detail.pre_deducted')}
                  </dt>
                  <dd>{formatCredits(detail.pre_deducted ?? 0, locale)}</dd>
                </div>
                <div className="flex justify-between gap-2">
                  <dt className="text-ink-2">{t('logs.usage.detail.settled')}</dt>
                  <dd>{formatCredits(detail.cost, locale)}</dd>
                </div>
                <div className="flex justify-between gap-2">
                  <dt className="text-ink-2">{t('logs.usage.detail.refunded')}</dt>
                  <dd>{formatCredits(detail.refunded, locale)}</dd>
                </div>
              </dl>
              {conclusionText !== null ? (
                <p className="mt-3 rounded-xl bg-surface-subtle p-3 text-sm text-brand">
                  {conclusionText}
                </p>
              ) : null}
            </Section>
          ) : null}

          {/* D. Timing & failure */}
          <Section title={t('logs.usage.detail.timing')}>
            <p className="mb-3 text-sm text-ink-1">
              {detail.status === 'success'
                ? t('logs.usage.detail.attempt_summary_success', {
                    count: attempts,
                    latency: formatSeconds(detail.latency_ms).replace('s', ''),
                  })
                : t('logs.usage.detail.attempt_summary_failed', {
                    count: attempts,
                    error: failureClass,
                    latency: formatSeconds(detail.latency_ms).replace('s', ''),
                  })}
            </p>
            <dl className="grid grid-cols-1 gap-x-6 gap-y-2 sm:grid-cols-2">
              <Field label={t('logs.usage.detail.latency')}>
                {formatSeconds(detail.latency_ms)}
              </Field>
              <Field label={t('logs.usage.detail.ttft')}>
                {detail.stream ? formatSeconds(detail.ttft) : '—'}
              </Field>
              <Field label={t('logs.usage.detail.retry_count_label')}>
                {t('logs.usage.detail.retry_count', { count: retries })}
              </Field>
            </dl>

            {detail.status !== 'success' ? (
              <div className="mt-3">
                <p className="text-[13px] font-medium text-ink-1">
                  {t('logs.usage.detail.failure_reason')}
                </p>
                <p className="mt-1 text-sm text-ink-2">
                  <span className="mr-2 rounded bg-danger-bg px-1.5 py-0.5 text-xs text-danger">
                    {detail.error_class ?? '—'}
                  </span>
                  {detail.error_message !== null &&
                  detail.error_message !== undefined
                    ? revealSensitive && capabilities.canViewSensitive
                      ? detail.error_message
                      : // 关闭开关时客户端同样打码：即使某天服务端契约回归，
                        // 也不会把原文渲染到屏幕上。
                        t('logs.usage.detail.masked')
                    : '—'}
                </p>
              </div>
            ) : null}

            {retryChain.length > 0 ? (
              <ol
                aria-label={t('logs.usage.detail.retry_chain')}
                className="mt-4 flex flex-col gap-2"
                data-testid="retry-chain"
              >
                <li className="text-[13px] font-medium text-ink-1">
                  {t('logs.usage.detail.retry_chain')}
                </li>
                {retryChain.map((step, index) => {
                  const record = asRecord(step)
                  const channel = readString(record, [
                    'channel_name',
                    'upstream',
                    'channel',
                  ])
                  // `decision` is an internal routing key. Non-admins only see
                  // the user-facing label (`decision_label`), never the raw
                  // priority/weight decision (G7-R47 element projection).
                  const decision = capabilities.isAdmin
                    ? readString(record, ['decision_label', 'decision'])
                    : readString(record, ['decision_label'])
                  const error = readString(record, ['error_class', 'error'])
                  const latency = readNumber(record, ['latency_ms', 'latency'])
                  const ok = error === undefined
                  return (
                    <li
                      key={index}
                      data-testid="retry-step"
                      className="flex flex-wrap items-center gap-2 rounded-lg bg-surface-subtle px-3 py-2 text-[13px]"
                    >
                      <span
                        aria-hidden="true"
                        className={[
                          'size-2 rounded-full',
                          ok ? 'bg-success' : 'bg-danger',
                        ].join(' ')}
                      />
                      <span className="font-medium">
                        {t('logs.usage.detail.attempt', { index: index + 1 })}
                      </span>
                      <span className="font-mono">{channel ?? '—'}</span>
                      {decision !== undefined ? (
                        <span className="text-ink-3">{decision}</span>
                      ) : null}
                      <span className={ok ? 'text-success' : 'text-danger'}>
                        {ok
                          ? t('logs.usage.detail.success')
                          : revealSensitive
                            ? error
                            : t('logs.usage.detail.masked')}
                      </span>
                      {latency !== undefined ? (
                        <span className="text-ink-3">{formatSeconds(latency)}</span>
                      ) : null}
                    </li>
                  )
                })}
              </ol>
            ) : null}
          </Section>

          {/* §6.4/§8.2：无 log.prompt.view 时连打码占位与「去筛选栏开启」
              引导都不渲染——入口本身不得暴露 Prompt 记录的存在性。 */}
          {capabilities.canViewPrompt &&
          (promptField !== undefined || revealSensitive) ? (
            <Section title={t('logs.usage.detail.prompt')}>
              {revealSensitive && promptField !== undefined ? (
                <p
                  data-testid="prompt-revealed"
                  className="max-h-[60vh] overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-surface-subtle p-3 font-mono text-sm"
                >
                  {promptField}
                </p>
              ) : promptField !== undefined ? (
                <>
                  <p className="font-mono text-sm" data-testid="prompt-masked">
                    {t('logs.usage.detail.masked_prompt')}
                  </p>
                  <p className="mt-1 text-xs text-ink-3">
                    {t('logs.usage.sensitive.enable_hint')}
                  </p>
                </>
              ) : (
                <p className="text-sm text-ink-2">
                  {t('logs.usage.sensitive.recording_disabled')}
                </p>
              )}
              {capabilities.canViewPrompt ? (
                <Button
                  variant="secondary"
                  className="mt-3"
                  disabled={promptRequesting || revealSensitive}
                  onClick={() => {
                    void openPrompt()
                  }}
                >
                  {t('logs.usage.detail.view_prompt')}
                </Button>
              ) : null}
            </Section>
          ) : null}
        </div>
      )}
    </Dialog>
  )
}

function TokenStat({ value, total }: { value: number; total: number }) {
  const { t } = useI18n()
  return (
    <span className="font-mono tabular-nums">
      {formatTokenAmount(value)}
      <span className="ml-2 text-[12px] text-ink-3">
        {t('logs.usage.detail.percent', {
          value: formatPercent(value, total),
        })}
      </span>
    </span>
  )
}
