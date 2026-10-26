/**
 * 向导第 1 步：搜索、全选/清空与模型事实表。
 *
 * 只读呈现 `WizardRow`；勾选、筛选词与目录错误状态都由容器持有。目录
 * 读取失败时表格仍然渲染（本轮不写库），但顶部给出重试入口，容器同时
 * 锁住「下一步」。
 */
import { useMemo } from 'react'

import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import type { WizardRow } from './model-profile-wizard-logic'

type ModelMeta = components['schemas']['ModelMeta']

type ProfileState = 'new' | 'existing' | 'locked'
type FactKind = 'auto' | 'suggest' | 'empty'

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'

const PROFILE_BADGE_CLASSES: Record<ProfileState, string> = {
  new: 'border-border-default bg-surface-subtle text-ink-2',
  existing: 'border-info-line bg-info-bg text-info',
  locked: 'border-border-default bg-surface-subtle text-ink-3',
}

const FACT_CHIP_CLASSES: Record<FactKind, string> = {
  auto: 'border-info-line bg-info-bg text-info',
  suggest: 'border-warning-line bg-warning-bg text-warning',
  empty: 'border-border-default bg-surface-subtle text-ink-3',
}

export interface StepModelsProps {
  rows: WizardRow[]
  catalog: ReadonlyMap<string, ModelMeta>
  query: string
  onQueryChange: (query: string) => void
  onToggleRow: (id: string) => void
  onSelectVisible: (ids: string[]) => void
  onClearAll: () => void
  catalogFailed: boolean
  onRetryCatalog: () => void
}

function displayNameOf(
  row: WizardRow,
  catalog: ReadonlyMap<string, ModelMeta>,
): string {
  if (row.displayName.trim() !== '') return row.displayName
  return catalog.get(row.id)?.display_name ?? ''
}

function profileStateOf(
  row: WizardRow,
  catalog: ReadonlyMap<string, ModelMeta>,
): ProfileState {
  const profile = catalog.get(row.id)
  if (profile === undefined) return 'new'
  return profile.sync_official ? 'existing' : 'locked'
}

/**
 * 事实来源只由「上游给没给值」推导，与 `facts_source`（上游目录种类）无关：
 * 自动 = 显示名 / 描述 / 上下文 / 最大输出 / 发布日期至少一项；
 * 建议 = 探测到了端点映射；两者都没有 = 待填。
 */
function factKindsOf(row: WizardRow): FactKind[] {
  const kinds: FactKind[] = []
  const hasFacts =
    row.displayName.trim() !== '' ||
    row.description.trim() !== '' ||
    row.contextLength !== '' ||
    row.maxOutput !== '' ||
    row.releasedAt !== ''
  if (hasFacts) kinds.push('auto')
  if (row.endpoints.length > 0) kinds.push('suggest')
  if (kinds.length === 0) kinds.push('empty')
  return kinds
}

export function StepModels({
  rows,
  catalog,
  query,
  onQueryChange,
  onToggleRow,
  onSelectVisible,
  onClearAll,
  catalogFailed,
  onRetryCatalog,
}: StepModelsProps) {
  const { t } = useI18n()
  const selectedCount = rows.filter((row) => row.selected).length
  // 搜索同时匹配模型 id 与展示名（含本地目录里的 display_name）。
  const filteredRows = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (needle === '') return rows
    return rows.filter(
      (row) =>
        row.id.toLowerCase().includes(needle) ||
        displayNameOf(row, catalog).toLowerCase().includes(needle),
    )
  }, [rows, query, catalog])

  return (
    <section className="flex flex-col gap-3">
      {catalogFailed ? (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-meta text-warning"
        >
          <span className="min-w-0 flex-1">
            {t('common.channels.profileWizard.catalogFailed')}
          </span>
          <Button variant="secondary" onClick={onRetryCatalog}>
            {t('common.channels.profileWizard.catalogRetry')}
          </Button>
        </div>
      ) : null}

      <div className="flex flex-wrap items-center gap-2">
        <input
          value={query}
          onChange={(event) => onQueryChange(event.target.value)}
          placeholder={t('common.channels.profileWizard.search')}
          aria-label={t('common.channels.profileWizard.search')}
          className={`${INPUT_CLASSES} min-w-0 flex-1`}
        />
        <Button
          variant="ghost"
          onClick={() => onSelectVisible(filteredRows.map((row) => row.id))}
        >
          {t('common.channels.profileWizard.selectAll')}
        </Button>
        <Button variant="ghost" onClick={onClearAll}>
          {t('common.channels.profileWizard.clearAll')}
        </Button>
      </div>

      <p className="text-meta text-ink-3">
        {t('common.channels.profileWizard.selectedCount', {
          count: selectedCount,
        })}
      </p>

      <div
        role="region"
        aria-label={t('common.channels.profileWizard.title')}
        tabIndex={0}
        className="overflow-x-auto rounded-lg border border-border-hairline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        <table className="w-full min-w-[620px] border-collapse text-meta">
          <caption className="sr-only">
            {t('common.channels.profileWizard.title')}
          </caption>
          <thead>
            <tr className="bg-surface-subtle text-left text-badge font-medium text-ink-3">
              <th scope="col" className="w-9 px-2 py-2">
                <span className="sr-only">
                  {t('common.channels.profileWizard.selectAll')}
                </span>
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.model')}
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.displayName')}
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.profileState')}
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.context')}
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.maxOutput')}
              </th>
              <th scope="col" className="px-2 py-2">
                {t('common.channels.profileWizard.factsSource')}
              </th>
            </tr>
          </thead>
          <tbody>
            {filteredRows.length === 0 ? (
              <tr>
                <td colSpan={7} className="px-4 py-8 text-center text-ink-3">
                  {t('common.channels.fetchModels.empty')}
                </td>
              </tr>
            ) : (
              filteredRows.map((row) => {
                const state = profileStateOf(row, catalog)
                const displayName = displayNameOf(row, catalog)
                return (
                  <tr
                    key={row.id}
                    className="border-t border-border-hairline hover:bg-surface-hover"
                  >
                    <td className="px-2 py-2">
                      <input
                        type="checkbox"
                        checked={row.selected}
                        onChange={() => onToggleRow(row.id)}
                        aria-label={row.id}
                        className="size-4 rounded border-border-default"
                      />
                    </td>
                    <td className="px-2 py-2">
                      <span className="font-mono text-ink-1">{row.id}</span>
                    </td>
                    <td className="max-w-[160px] truncate px-2 py-2 text-ink-2">
                      {displayName === '' ? '—' : displayName}
                    </td>
                    <td className="px-2 py-2">
                      <span
                        className={`inline-flex h-[21px] items-center rounded border px-1.5 text-badge whitespace-nowrap ${PROFILE_BADGE_CLASSES[state]}`}
                      >
                        {t(`common.channels.profileWizard.state.${state}`)}
                      </span>
                    </td>
                    <td className="px-2 py-2 tabular-nums text-ink-2">
                      {row.contextLength === '' ? '—' : row.contextLength}
                    </td>
                    <td className="px-2 py-2 tabular-nums text-ink-2">
                      {row.maxOutput === '' ? '—' : row.maxOutput}
                    </td>
                    <td className="px-2 py-2">
                      <span className="flex flex-wrap gap-1">
                        {factKindsOf(row).map((kind) => (
                          <span
                            key={kind}
                            className={`inline-flex h-[21px] items-center gap-1 rounded border px-1.5 text-badge whitespace-nowrap ${FACT_CHIP_CLASSES[kind]}`}
                          >
                            <span
                              aria-hidden="true"
                              className="h-3 w-0.5 rounded-full bg-current"
                            />
                            {t(`common.channels.profileWizard.fact.${kind}`)}
                          </span>
                        ))}
                      </span>
                    </td>
                  </tr>
                )
              })
            )}
          </tbody>
        </table>
      </div>
    </section>
  )
}
