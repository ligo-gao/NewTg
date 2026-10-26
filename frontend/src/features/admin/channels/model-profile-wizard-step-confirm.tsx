/**
 * 向导第 3 步：逐行确认。
 *
 * 每行默认收起，摘要只显示模型 id、档案状态、命中动作（新建 / 更新 / 跳过）
 * 与输入错误标记。展开后是该行的字段编辑器（见
 * `model-profile-wizard-step-confirm-row.tsx`）：初值来自探测事实、既有档案
 * 与第 2 步套用的默认值（行状态由容器持有），行内可改。
 *
 * 本地锁定（`sync_official === false`）的行不可展开、不可覆盖，只显示说明；
 * 「覆盖」复选框只对既有档案出现，锁定时禁用。
 */
import { useState } from 'react'

import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { FormAlert } from '@/shared/ui/form'

import {
  MAX_WIZARD_ROWS,
  rowActionOf,
  rowIssues,
  vendorStateOf,
  type RowIssue,
  type WizardRow,
} from './model-profile-wizard-logic'
import type { VendorDirectory } from './model-profile-wizard-queries'
import { StepConfirmRowEditor } from './model-profile-wizard-step-confirm-row'

type ModelMeta = components['schemas']['ModelMeta']

type ProfileState = 'new' | 'existing' | 'locked'

const PROFILE_BADGE_CLASSES: Record<ProfileState, string> = {
  new: 'border-border-default bg-surface-subtle text-ink-2',
  existing: 'border-info-line bg-info-bg text-info',
  locked: 'border-border-default bg-surface-subtle text-ink-3',
}

const ACTION_BADGE_CLASSES = {
  create: 'border-success-line bg-success-bg text-success',
  update: 'border-warning-line bg-warning-bg text-warning',
  skip: 'border-border-default bg-surface-subtle text-ink-3',
} as const

/** 摘要上的错误标记用字段名做 aria-label，管理员据此决定展开哪行。 */
const ISSUE_LABEL_KEYS: Record<RowIssue, string> = {
  displayName: 'common.channels.profileWizard.displayName',
  description: 'common.channels.profileWizard.description',
  contextLength: 'common.channels.profileWizard.context',
  maxOutput: 'common.channels.profileWizard.maxOutput',
  knowledgeCutoff: 'common.channels.profileWizard.knowledgeCutoff',
  parameters: 'common.channels.profileWizard.parameters',
}

export interface StepConfirmProps {
  /** 只包含已勾选的行（容器过滤）。 */
  rows: WizardRow[]
  catalog: ReadonlyMap<string, ModelMeta>
  vendorDirectory: VendorDirectory
  /** 第 2 步显式填写的新建供应商名（空串 = 没有选择新建）。 */
  newVendorName: string
  tooMany: boolean
  onUpdateRow: (id: string, patch: Partial<WizardRow>) => void
}

function profileStateOf(profile: ModelMeta | undefined): ProfileState {
  if (profile === undefined) return 'new'
  return profile.sync_official ? 'existing' : 'locked'
}

export function StepConfirm({
  rows,
  catalog,
  vendorDirectory,
  newVendorName,
  tooMany,
  onUpdateRow,
}: StepConfirmProps) {
  const { t } = useI18n()
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())

  function toggleExpanded(id: string): void {
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  if (rows.length === 0) {
    return (
      <div className="flex min-h-[120px] items-center justify-center text-body text-ink-3">
        {t('common.channels.profileWizard.emptySelection')}
      </div>
    )
  }

  return (
    <section
      aria-label={t('common.channels.profileWizard.stepConfirm')}
      className="flex flex-col gap-2"
    >
      <h3 className="text-body font-semibold text-ink-1">
        {t('common.channels.profileWizard.stepConfirm')}
      </h3>

      {tooMany ? (
        <FormAlert tone="warning">
          {t('common.channels.profileWizard.tooMany', {
            max: MAX_WIZARD_ROWS,
          })}
        </FormAlert>
      ) : null}

      {rows.map((row) => {
        const profile = catalog.get(row.id)
        const state = profileStateOf(profile)
        const locked = state === 'locked'
        const action = rowActionOf(row, profile)
        const issues = rowIssues(row)
        const open = expanded.has(row.id) && !locked
        const vendorState = vendorStateOf(
          row.vendorName,
          vendorDirectory,
          newVendorName,
        )

        return (
          <div
            key={row.id}
            className="rounded-lg border border-border-hairline bg-surface-card"
          >
            <div className="flex flex-wrap items-center gap-2 px-3 py-2">
              {locked ? (
                <span className="font-mono text-meta text-ink-1">{row.id}</span>
              ) : (
                <Button
                  variant="ghost"
                  aria-expanded={open}
                  className="!px-1.5"
                  onClick={() => {
                    toggleExpanded(row.id)
                  }}
                >
                  <span aria-hidden="true" className="text-ink-3">
                    {open ? '▾' : '▸'}
                  </span>
                  <span className="font-mono">{row.id}</span>
                </Button>
              )}

              <span
                className={`inline-flex h-[21px] items-center rounded border px-1.5 text-badge whitespace-nowrap ${PROFILE_BADGE_CLASSES[state]}`}
              >
                {state === 'locked'
                  ? t('common.channels.profileWizard.locked')
                  : t(`common.channels.profileWizard.state.${state}`)}
              </span>

              <span
                className={`inline-flex h-[21px] items-center rounded border px-1.5 text-badge whitespace-nowrap ${ACTION_BADGE_CLASSES[action]}`}
              >
                {t(`common.channels.profileWizard.action.${action}`)}
              </span>

              {issues.length === 0 ? null : (
                <span
                  role="img"
                  aria-label={issues
                    .map((issue) => t(ISSUE_LABEL_KEYS[issue]))
                    .join(', ')}
                  className="inline-flex size-[18px] items-center justify-center rounded-full border border-danger-line bg-danger-bg text-badge font-semibold text-danger"
                >
                  !
                </span>
              )}

              {profile === undefined ? null : (
                <label className="ml-auto flex items-center gap-2 text-meta text-ink-2">
                  <input
                    type="checkbox"
                    aria-label={t('common.channels.profileWizard.overwrite')}
                    checked={!locked && row.overwrite}
                    disabled={locked}
                    onChange={(event) => {
                      onUpdateRow(row.id, { overwrite: event.target.checked })
                    }}
                    className="size-4 rounded border-border-default"
                  />
                  {t('common.channels.profileWizard.overwrite')}
                </label>
              )}
            </div>

            {locked ? null : vendorState.kind === 'new' ? (
              <p className="px-3 pb-2 text-meta text-ink-3">
                {t('common.channels.profileWizard.vendorWillCreate', {
                  name: vendorState.name,
                })}
              </p>
            ) : vendorState.kind === 'unresolved' ? (
              <p className="px-3 pb-2 text-meta text-warning">
                {t('common.channels.profileWizard.vendorSuggestionUnresolved', {
                  name: vendorState.name,
                })}
              </p>
            ) : null}

            {locked ? (
              <p className="border-t border-border-hairline px-3 py-2 text-meta text-ink-3">
                {t('common.channels.profileWizard.lockedHint')}
              </p>
            ) : null}

            {open ? (
              <StepConfirmRowEditor
                row={row}
                vendorDirectory={vendorDirectory}
                newVendorName={newVendorName}
                onUpdateRow={onUpdateRow}
              />
            ) : null}
          </div>
        )
      })}
    </section>
  )
}
