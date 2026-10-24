import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { Tag } from '@/shared/ui/tag'

import type { PricingMode } from './pricing-utils'
import { pricingModeKey } from './pricing-utils'

export interface PricePreviewProps {
  mode: PricingMode
  expr: string
  promptPrice: number | null
  completionPrice: number | null
  cacheReadPrice: number | null
  cacheWritePrice: number | null
  model: string
  /** 防抖毫秒数；默认 300，避免每次击键都打一次求值器。 */
  debounceMs?: number
  /**
   * 让抽屉把「表达式非法」红标打在编辑框本身，而不只是预览面板内
   * （F-06-14 的校验定位要求），因为预览端点才是权威解析器。
   */
  onValidationChange?: (invalid: boolean) => void
}

/**
 * 构造 POST /api/model-pricing:preview 的求值 spec。
 *
 * 预览契约接收与定价解析器相同的 spec 对象，因此 per_request 行必须发
 * model_price（解析器的按次键），尽管存储列叫 prompt_price。按 token 的
 * 绝对价与 prompt_price / completion_price / ... 一一对应，因为存储单位
 * 本来就是「积分 / 1M tokens」。
 *
 * 还没有可求值内容时返回 null：空表达式，或操作者尚未填写按次价格
 * （存储的 NULL 由服务端按模型族默认兜底，本编辑器刻意不猜）。
 */
function buildPreviewSpec(props: PricePreviewProps): Record<string, unknown> | null {
  if (props.mode === 'tiered_expr') {
    const expr = props.expr.trim()
    if (expr === '') return null
    return { mode: 'tiered_expr', expr }
  }
  if (props.mode === 'per_request') {
    if (props.promptPrice === null) return null
    return { mode: 'per_request', model_price: props.promptPrice }
  }
  const spec: Record<string, unknown> = { mode: 'per_token' }
  if (props.promptPrice !== null) spec.prompt_price = props.promptPrice
  if (props.completionPrice !== null) spec.completion_price = props.completionPrice
  if (props.cacheReadPrice !== null) spec.cache_read_price = props.cacheReadPrice
  if (props.cacheWritePrice !== null) spec.cache_write_price = props.cacheWritePrice
  return spec
}

/**
 * 实时求值面板（F-06-14）。非法表达式由父抽屉在字段处标红；本组件展示
 * 求值结果（积分、元、免费/饱和徽章），并区分校验拒绝（红色，「改表达式」）
 * 与暂时性失败（可重试）。
 */
export function PricePreview({
  mode,
  expr,
  promptPrice,
  completionPrice,
  cacheReadPrice,
  cacheWritePrice,
  model,
  debounceMs = 300,
  onValidationChange,
}: PricePreviewProps) {
  const { t } = useI18n()
  const spec = buildPreviewSpec({
    mode,
    expr,
    promptPrice,
    completionPrice,
    cacheReadPrice,
    cacheWritePrice,
    model,
  })
  const specJson = spec === null ? null : JSON.stringify(spec)
  const [settledJson, setSettledJson] = useState<string | null>(specJson)

  useEffect(() => {
    if (specJson === null) {
      setSettledJson(null)
      return
    }
    const timer = setTimeout(() => {
      setSettledJson(specJson)
    }, debounceMs)
    return () => {
      clearTimeout(timer)
    }
  }, [specJson, debounceMs])

  const previewQuery = useQuery({
    queryKey: ['admin-pricing-preview', settledJson, model],
    enabled: settledJson !== null,
    retry: false,
    staleTime: 0,
    queryFn: async () => {
      const parsed = JSON.parse(settledJson ?? '{}') as Record<string, unknown>
      return requireData(
        await apiClient.POST('/api/model-pricing:preview', {
          body: {
            spec: parsed,
            group_ratio: 1,
            ...(model.trim() === '' ? {} : { model: model.trim() }),
            // 100 万输入 + 100 万输出、图片张数未设置，是一个稳定且好读的样例。
            params: { p: 1_000_000, c: 1_000_000, image_count: -1 },
          },
        }),
      )
    },
  })

  const error = previewQuery.error
  const validationError = error instanceof ApiError && error.kind === 'validation'

  useEffect(() => {
    onValidationChange?.(validationError)
  }, [validationError, onValidationChange])

  return (
    <section
      aria-label={t('admin.pricing.preview_title')}
      className="flex flex-col gap-2 rounded-lg border border-border-hairline bg-surface-subtle p-3"
    >
      <div className="flex items-center justify-between">
        <h3 className="text-body font-semibold text-ink-1">
          {t('admin.pricing.preview_title')}
        </h3>
        <span className="text-meta text-ink-3">{t('admin.pricing.preview_sample')}</span>
      </div>

      {specJson === null ? (
        <p className="text-meta text-ink-3">{t('admin.pricing.preview_wait')}</p>
      ) : previewQuery.isPending || settledJson === null ? (
        <p className="text-meta text-ink-3" aria-busy="true">
          {t('admin.pricing.preview_loading')}
        </p>
      ) : previewQuery.isError ? (
        <div className="flex flex-col gap-2" role="alert">
          <p className="text-body text-danger">
            {validationError
              ? mode === 'tiered_expr'
                ? t('admin.pricing.expression_invalid')
                : t('admin.pricing.preview_invalid_input')
              : t('admin.pricing.preview_failed')}
          </p>
          {validationError ? null : (
            <Button
              variant="secondary"
              className="h-7 self-start px-2 text-meta"
              onClick={() => void previewQuery.refetch()}
            >
              {t('admin.pricing.preview_retry')}
            </Button>
          )}
        </div>
      ) : previewQuery.data !== undefined ? (
        <div className="flex flex-wrap items-center gap-2 text-body text-ink-1">
          <span className="font-mono tabular-nums" data-testid="pricing-preview-quota">
            {t('admin.pricing.preview_quota', {
              quota: formatCredits(previewQuery.data.quota),
            })}
          </span>
          <span className="text-ink-2">
            {t('admin.pricing.preview_yuan', { amount: previewQuery.data.amount_yuan })}
          </span>
          <Tag ariaLabel={t(pricingModeKey(previewQuery.data.mode))}>
            {t(pricingModeKey(previewQuery.data.mode))}
          </Tag>
          {previewQuery.data.free_model ? (
            <Tag variant="success" ariaLabel={t('admin.pricing.free')}>
              {t('admin.pricing.free')}
            </Tag>
          ) : null}
          {previewQuery.data.clamped ? (
            <Tag variant="warning">{t('admin.pricing.preview_clamped')}</Tag>
          ) : null}
        </div>
      ) : null}
    </section>
  )
}
