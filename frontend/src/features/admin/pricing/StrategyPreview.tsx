import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Tag } from '@/shared/ui/tag'

import {
  formatMultiplierPercent,
  matchLocalTimeRule,
  parseMultiplierPercent,
  wallClockParts,
  type PricingMode,
  type TimeConfigDraft,
} from './pricing-utils'

export interface StrategyPreviewPrices {
  prompt: number | null
  completion: number | null
  cacheRead: number | null
  cacheWrite: number | null
}

export interface StrategyPreviewProps {
  mode: PricingMode
  prices: StrategyPreviewPrices
  config: TimeConfigDraft
  /** 当前试算时间（`datetime-local` 墙钟值）。 */
  at: string
  onAtChange: (value: string) => void
  quotaPerUnit: number
}

interface TokenInput {
  input: string
  output: string
  cacheRead: string
}

const DEFAULT_TOKENS: TokenInput = {
  input: '128000',
  output: '4000',
  cacheRead: '64000',
}

function toTokens(raw: string): number {
  const value = Number(raw)
  if (!Number.isFinite(value) || value < 0) return 0
  return Math.floor(value)
}

function credits4(value: number): string {
  if (!Number.isFinite(value)) return '0'
  return String(Number(value.toFixed(4)))
}

function yuan4(value: number, quotaPerUnit: number): string {
  const factor = quotaPerUnit > 0 ? quotaPerUnit : 100
  if (!Number.isFinite(value)) return '0.0000'
  return (value / factor).toFixed(4)
}

/**
 * 本地试算面板（F-06-14）。用与后端同一套结构化规则（窗口/跨夜/默认倍率）
 * 即时算出各分项成本；节假日只有服务端持有日历，保存前抽屉会调用
 * `POST /api/model-pricing:preview-strategy` 做一致性校验。
 *
 * 面板不发请求：每次输入都在本地重算，避免击键打满预览端点。
 */
export function StrategyPreview({
  mode,
  prices,
  config,
  at,
  onAtChange,
  quotaPerUnit,
}: StrategyPreviewProps) {
  const { t } = useI18n()
  const [tokens, setTokens] = useState<TokenInput>(DEFAULT_TOKENS)

  const wallClock = wallClockParts(at)
  const match =
    config.enabled && wallClock !== null
      ? matchLocalTimeRule(config, wallClock.day, wallClock.minutes)
      : { window: '', multiplier: 1 }
  const fallback = parseMultiplierPercent(config.defaultMultiplier)
  const defaultMultiplier = fallback.invalid ? 1 : (fallback.value ?? 1)

  const inputTokens = toTokens(tokens.input)
  const outputTokens = toTokens(tokens.output)
  const cacheReadTokens = toTokens(tokens.cacheRead)

  const prompt = prices.prompt ?? 0
  const completion = prices.completion ?? 0
  const cacheRead = prices.cacheRead ?? 0

  let total = 0
  let inputCost = 0
  let outputCost = 0
  let cacheCost = 0
  if (mode === 'per_request') {
    total = prompt * match.multiplier
  } else {
    inputCost = (inputTokens * prompt * match.multiplier) / 1_000_000
    outputCost = (outputTokens * completion * match.multiplier) / 1_000_000
    cacheCost = (cacheReadTokens * cacheRead * match.multiplier) / 1_000_000
    total = inputCost + outputCost + cacheCost
  }

  const ruleLabel =
    !config.enabled
      ? t('admin.pricing.preview_time_disabled')
      : match.window === ''
        ? t('admin.pricing.preview_rule_default')
        : match.window
  const kindTag =
    config.enabled && Math.abs(match.multiplier - defaultMultiplier) > 1e-9
      ? match.multiplier > defaultMultiplier
        ? t('admin.pricing.band_legend_peak')
        : t('admin.pricing.band_legend_off')
      : t('admin.pricing.band_legend_base')

  return (
    <section
      aria-label={t('admin.pricing.strategy_preview_title')}
      className="flex flex-col gap-3 rounded-lg border border-border-hairline bg-surface-subtle p-3"
    >
      <div className="flex flex-col gap-1">
        <h3 className="text-sm font-semibold text-ink-1">
          {t('admin.pricing.strategy_preview_title')}
        </h3>
        <p className="text-xs text-ink-3">{t('admin.pricing.strategy_preview_hint')}</p>
      </div>

      <div className="flex flex-col gap-1.5">
        <label
          htmlFor="strategy-preview-at"
          className="text-[13px] font-medium text-ink-1"
        >
          {t('admin.pricing.preview_at')}
        </label>
        <input
          id="strategy-preview-at"
          type="datetime-local"
          value={at}
          className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          onChange={(event) => {
            onAtChange(event.target.value)
          }}
        />
        <div className="flex flex-wrap items-center gap-2 text-xs text-ink-2">
          <Tag variant="info">{ruleLabel}</Tag>
          <Tag variant="neutral">{kindTag}</Tag>
          <span className="font-mono tabular-nums">
            {t('admin.pricing.preview_multiplier', {
              multiplier: formatMultiplierPercent(match.multiplier),
            })}
          </span>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
        <TokenField
          label={t('admin.pricing.preview_input_tokens')}
          value={tokens.input}
          onChange={(value) => {
            setTokens((current) => ({ ...current, input: value }))
          }}
        />
        <TokenField
          label={t('admin.pricing.preview_output_tokens')}
          value={tokens.output}
          onChange={(value) => {
            setTokens((current) => ({ ...current, output: value }))
          }}
        />
        <TokenField
          label={t('admin.pricing.preview_cache_tokens')}
          value={tokens.cacheRead}
          onChange={(value) => {
            setTokens((current) => ({ ...current, cacheRead: value }))
          }}
        />
      </div>

      <div
        aria-live="polite"
        className="flex flex-wrap items-baseline justify-between gap-2 rounded-lg border border-border-hairline bg-surface-card px-3 py-2"
      >
        <span className="text-xs text-ink-3">
          {t('admin.pricing.strategy_preview_total')}
        </span>
        <span className="flex items-baseline gap-2">
          <span
            data-testid="strategy-preview-total-credits"
            className="font-mono text-sm tabular-nums text-ink-1"
          >
            {t('admin.pricing.preview_credits', { credits: credits4(total) })}
          </span>
          <span
            data-testid="strategy-preview-total-yuan"
            className="font-mono text-sm tabular-nums text-ink-1"
          >
            {t('admin.pricing.preview_yuan4', {
              amount: yuan4(total, quotaPerUnit),
            })}
          </span>
        </span>
      </div>

      <div className="flex flex-col gap-1">
        <h4 className="text-[13px] font-medium text-ink-1">
          {t('admin.pricing.preview_breakdown')}
        </h4>
        {mode === 'per_request' ? (
          <BreakdownRow
            label={t('admin.pricing.preview_per_request_line', {
              rate: credits4(prompt * match.multiplier),
            })}
            amount={credits4(total)}
          />
        ) : (
          <>
            <BreakdownRow
              label={t('admin.pricing.preview_input_line', {
                tokens: formatCredits(inputTokens),
                rate: credits4(prompt * match.multiplier),
              })}
              amount={credits4(inputCost)}
            />
            <BreakdownRow
              label={t('admin.pricing.preview_output_line', {
                tokens: formatCredits(outputTokens),
                rate: credits4(completion * match.multiplier),
              })}
              amount={credits4(outputCost)}
            />
            <BreakdownRow
              label={t('admin.pricing.preview_cache_line', {
                tokens: formatCredits(cacheReadTokens),
                rate: credits4(cacheRead * match.multiplier),
              })}
              amount={credits4(cacheCost)}
            />
          </>
        )}
      </div>

      {mode !== 'per_request' ? (
        <div className="grid grid-cols-1 gap-1 text-xs text-ink-2 sm:grid-cols-2">
          <span>
            {t('admin.pricing.preview_effective', {
              rate: credits4(prompt * match.multiplier),
            })}
          </span>
          <span>
            {t('admin.pricing.preview_effective_output', {
              rate: credits4(completion * match.multiplier),
            })}
          </span>
        </div>
      ) : null}

      {config.enabled && config.holidayPolicy === 'cn_offpeak' ? (
        <p className="text-xs text-ink-3">
          {t('admin.pricing.preview_holiday_note')}
        </p>
      ) : null}
    </section>
  )
}

function TokenField({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (value: string) => void
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[13px] font-medium text-ink-1">{label}</span>
      <input
        type="number"
        min={0}
        step={1}
        inputMode="numeric"
        value={value}
        className="h-9 w-full rounded-lg border border-border-default bg-surface-card px-2 text-sm tabular-nums text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        onChange={(event) => {
          onChange(event.target.value)
        }}
      />
    </label>
  )
}

function BreakdownRow({ label, amount }: { label: string; amount: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2 text-xs text-ink-2">
      <span>{label}</span>
      <span className="font-mono tabular-nums text-ink-1">{amount}</span>
    </div>
  )
}
