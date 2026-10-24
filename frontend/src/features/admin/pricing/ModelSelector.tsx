import { useId } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

type PricingModelOption = components['schemas']['PricingModelOption']

export interface ModelSelectorProps {
  label: string
  value: string
  onChange: (model: string, option: PricingModelOption | undefined) => void
  /** 分组覆盖价可以指向已停用的源模型（表格行会给出警告）。 */
  includeDisabled?: boolean
  /**
   * 表达式源模型不读取分组覆盖价（服务端会拒绝写入），因此覆盖价抽屉把
   * 这类模型过滤掉。已有行仍把当前值保留为额外选项并给出警告。
   */
  excludeExpression?: boolean
  disabled?: boolean
  error?: string
}

/**
 * 由 GET /api/model-pricing/models 驱动的模型下拉（F-06-14/F-06-16）。
 * 端点上限 1000 行，管理侧定价目录预期远低于该值。之前选中但已不在响应
 * 里的值会保留为额外选项，避免编辑已有行时被静默清空。
 */
export function ModelSelector({
  label,
  value,
  onChange,
  includeDisabled = false,
  excludeExpression = false,
  disabled = false,
  error,
}: ModelSelectorProps) {
  const { t } = useI18n()
  const id = useId()
  const errorId = `${id}-error`

  const modelsQuery = useQuery({
    queryKey: ['admin-pricing-models', includeDisabled ? 'all' : 'enabled'],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/model-pricing/models', {
          params: {
            query: {
              status: includeDisabled ? 'all' : 'enabled',
              limit: 1000,
            },
          },
        }),
      ),
    staleTime: 30_000,
  })

  const rawOptions = modelsQuery.data?.items ?? []
  const options = excludeExpression
    ? rawOptions.filter((option) => option.mode !== 'tiered_expr')
    : rawOptions
  const known = options.some((option) => option.model === value)
  const currentOption = rawOptions.find((option) => option.model === value)

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-meta font-medium text-ink-1">
        {label}
      </label>
      <select
        id={id}
        className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        value={value}
        disabled={disabled || modelsQuery.isPending}
        aria-invalid={error !== undefined ? true : undefined}
        aria-describedby={error !== undefined ? errorId : undefined}
        onChange={(event) => {
          const next = event.target.value
          onChange(next, options.find((option) => option.model === next))
        }}
      >
        <option value="">
          {modelsQuery.isPending
            ? t('admin.pricing.models_loading')
            : t('admin.pricing.models_placeholder')}
        </option>
        {value !== '' && !known ? <option value={value}>{value}</option> : null}
        {options.map((option) => (
          <option key={option.model} value={option.model}>
            {option.status === 2
              ? `${option.model} (${t('admin.pricing.status_disabled')})`
              : option.model}
          </option>
        ))}
      </select>
      {modelsQuery.isError ? (
        <div className="flex items-center gap-2 text-meta text-danger">
          <span>{t('admin.pricing.models_error')}</span>
          <Button
            variant="ghost"
            className="h-6 px-1 text-meta"
            onClick={() => void modelsQuery.refetch()}
          >
            {t('common.action.retry')}
          </Button>
        </div>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-meta text-danger">
          {error}
        </p>
      ) : null}
      {currentOption !== undefined && currentOption.status === 2 ? (
        <p className="text-meta text-warning">{t('admin.pricing.models_source_disabled')}</p>
      ) : null}
      {currentOption !== undefined && currentOption.mode === 'tiered_expr' ? (
        <p className="text-meta text-warning">{t('admin.pricing.models_source_expression')}</p>
      ) : null}
    </div>
  )
}
