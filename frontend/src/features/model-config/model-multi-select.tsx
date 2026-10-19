import { useId, useMemo, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import type { UsableGroup } from './api'

export interface ModelMultiSelectProps {
  /** 可用分组；候选模型只从这些分组里产生（§2.15 安全纪律）。 */
  groups: readonly UsableGroup[]
  /** 已选模型；空数组 = 不限。 */
  value: readonly string[]
  onChange: (next: string[]) => void
  /** 可访问名称（fieldset legend 的替代）。 */
  label: string
  /** 候选为空时的提示文案。 */
  emptyLabel: string
  idPrefix?: string
}

/**
 * 模型多选器（F-02-18 的行内/批量编辑，复用 §2.15 组件树里的
 * ModelMultiSelect 语义）。
 *
 * 为什么用「分组分区 + 复选」而不是逗号分隔输入：模型名是受控枚举，
 * 自由文本会把拼写错误留给服务端 422；按分组展示还能让用户直观看到
 * 某个模型属于哪个可用分组。选中集合由调用方持有，本组件无隐藏状态。
 */
export function ModelMultiSelect({
  groups,
  value,
  onChange,
  label,
  emptyLabel,
  idPrefix,
}: ModelMultiSelectProps) {
  const { t } = useI18n()
  const generatedId = useId()
  const prefix = idPrefix ?? generatedId
  const [query, setQuery] = useState('')

  const selected = useMemo(() => new Set(value), [value])
  const normalizedQuery = query.trim().toLowerCase()

  const visibleGroups = useMemo(
    () =>
      groups
        .map((group) => ({
          name: group.name,
          models: group.models.filter(
            (model) =>
              normalizedQuery === '' ||
              model.toLowerCase().includes(normalizedQuery),
          ),
        }))
        .filter((group) => group.models.length > 0),
    [groups, normalizedQuery],
  )

  function toggle(model: string, checked: boolean): void {
    if (checked) {
      if (selected.has(model)) return
      onChange([...value, model])
      return
    }
    onChange(value.filter((item) => item !== model))
  }

  return (
    <div className="flex flex-col gap-2">
      <label
        htmlFor={`${prefix}-search`}
        className="text-[13px] font-medium text-ink-1"
      >
        {t('account.model_config.keys.search_models')}
      </label>
      <input
        id={`${prefix}-search`}
        type="search"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        placeholder={t('account.model_config.keys.search_models_placeholder')}
        className="h-9 w-full rounded-lg border border-border-default bg-surface-page px-3 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      />
      <div
        role="group"
        aria-label={label}
        className="max-h-72 overflow-y-auto rounded-lg border border-border-hairline bg-surface-page p-3"
      >
        {visibleGroups.length === 0 ? (
          <p className="text-[13px] text-ink-3">{emptyLabel}</p>
        ) : (
          visibleGroups.map((group) => (
            <fieldset key={group.name} className="mb-3 last:mb-0">
              <legend className="mb-1 text-xs font-semibold text-ink-3">
                {group.name}
              </legend>
              <div className="grid grid-cols-1 gap-1 sm:grid-cols-2">
                {group.models.map((model) => {
                  const id = `${prefix}-${group.name}-${model}`
                  return (
                    <label
                      key={model}
                      htmlFor={id}
                      className="flex items-center gap-2 rounded px-1 py-0.5 text-[13px] text-ink-1 hover:bg-surface-hover"
                    >
                      <input
                        id={id}
                        type="checkbox"
                        checked={selected.has(model)}
                        onChange={(event) =>
                          toggle(model, event.target.checked)
                        }
                        className="size-4 accent-brand"
                      />
                      <span className="truncate font-mono text-xs">{model}</span>
                    </label>
                  )
                })}
              </div>
            </fieldset>
          ))
        )}
      </div>
      <p className="text-xs text-ink-3">
        {t('account.model_config.keys.unlimited_hint')}
      </p>
    </div>
  )
}
