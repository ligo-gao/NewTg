/**
 * 向导第 3 步：逐行确认。
 *
 * 每行默认收起，摘要只显示模型 id、档案状态与命中动作（新建 / 更新 / 跳过）。
 * 展开后是该行的字段编辑器：初值来自第 2 步套用的默认值（行状态由容器
 * 持有），行内可改。这里不推断任何能力 / 模态 / 参数——上游没给的、管理员
 * 没填的就是空的。
 *
 * 本地锁定（`sync_official === false`）的行不可展开、不可覆盖，只显示说明；
 * 「覆盖」复选框只对既有档案出现，锁定时禁用。
 *
 * 校验文案必须自己报：写入体遇到非法数值 / 类型会静默丢字段。
 */
import { useState } from 'react'

import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  CAPABILITY_KEYS,
  DESCRIPTION_MAX,
  DISPLAY_NAME_MAX,
  ENDPOINT_KEYS,
  MAX_WIZARD_ROWS,
  MODALITY_KEYS,
  rowActionOf,
  rowIssues,
  type EndpointKey,
  type ParameterRow,
  type RowIssue,
  type WizardRow,
} from './model-profile-wizard-logic'
import type { VendorDirectory } from './model-profile-wizard-queries'
import { CREATE_VENDOR } from './model-profile-wizard-step-defaults'
import { ChipToggle } from './ui/chip-toggle'

type ModelMeta = components['schemas']['ModelMeta']

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'

const FIELD_LABEL_CLASSES = 'text-meta text-ink-2'

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

export interface StepConfirmProps {
  /** 只包含已勾选的行（容器过滤）。 */
  rows: WizardRow[]
  catalog: ReadonlyMap<string, ModelMeta>
  vendorDirectory: VendorDirectory
  tooMany: boolean
  onUpdateRow: (id: string, patch: Partial<WizardRow>) => void
}

function profileStateOf(profile: ModelMeta | undefined): ProfileState {
  if (profile === undefined) return 'new'
  return profile.sync_official ? 'existing' : 'locked'
}

function toggled<T>(list: T[], value: T): T[] {
  return list.includes(value)
    ? list.filter((item) => item !== value)
    : [...list, value]
}

function parseTags(text: string): string[] {
  return text
    .split(',')
    .map((tag) => tag.trim())
    .filter((tag) => tag !== '')
}

export function StepConfirm({
  rows,
  catalog,
  vendorDirectory,
  tooMany,
  onUpdateRow,
}: StepConfirmProps) {
  const { t } = useI18n()
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())
  // 供应商下拉的「提交时新建」没有可回读的值：选中后名称还是空的，因此把
  // 「正在新建」的行记在本地，改名输入框的值仍然落在 row.vendorName 上。
  const [creatingVendor, setCreatingVendor] = useState<ReadonlySet<string>>(
    new Set(),
  )
  // 标签输入保留原始文本，否则每次按键都会因 split / join 吞掉刚输入的逗号。
  const [tagDrafts, setTagDrafts] = useState<Record<string, string>>({})

  const vendorNames = vendorDirectory.names

  function toggleExpanded(id: string): void {
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function setVendorCreating(id: string, creating: boolean): void {
    setCreatingVendor((current) => {
      const next = new Set(current)
      if (creating) next.add(id)
      else next.delete(id)
      return next
    })
  }

  function changeVendor(row: WizardRow, value: string): void {
    if (value === CREATE_VENDOR) {
      setVendorCreating(row.id, true)
      onUpdateRow(row.id, { vendorName: '' })
      return
    }
    setVendorCreating(row.id, false)
    onUpdateRow(row.id, { vendorName: value })
  }

  function updateParameter(
    row: WizardRow,
    index: number,
    patch: Partial<ParameterRow>,
  ): void {
    onUpdateRow(row.id, {
      parameters: row.parameters.map((parameter, current) =>
        current === index ? { ...parameter, ...patch } : parameter,
      ),
    })
  }

  function removeParameter(row: WizardRow, index: number): void {
    onUpdateRow(row.id, {
      parameters: row.parameters.filter((_, current) => current !== index),
    })
  }

  function addParameter(row: WizardRow): void {
    onUpdateRow(row.id, {
      parameters: [
        ...row.parameters,
        { name: '', type: 'string', description: '', defaultValue: '' },
      ],
    })
  }

  function errorOf(
    issues: RowIssue[],
    issue: RowIssue,
    message: string,
  ): string | undefined {
    return issues.includes(issue) ? message : undefined
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
        const panelId = `profile-wizard-row-${row.id}`
        const vendorKey = row.vendorName.trim().toLowerCase()
        const vendorListed =
          vendorKey !== '' && vendorDirectory.ids.has(vendorKey)
        const vendorChoice = locked
          ? ''
          : creatingVendor.has(row.id)
            ? CREATE_VENDOR
            : vendorListed || vendorKey === ''
              ? row.vendorName.trim()
              : CREATE_VENDOR
        const displayError = errorOf(
          issues,
          'displayName',
          t('common.channels.profileWizard.displayNameTooLong', {
            count: DISPLAY_NAME_MAX,
          }),
        )
        const descriptionError = errorOf(
          issues,
          'description',
          t('common.channels.profileWizard.descriptionTooLong', {
            count: DESCRIPTION_MAX,
          }),
        )
        const contextError = errorOf(
          issues,
          'contextLength',
          t('common.channels.profileWizard.nonNegativeInteger'),
        )
        const maxOutputError = errorOf(
          issues,
          'maxOutput',
          t('common.channels.profileWizard.nonNegativeInteger'),
        )
        const cutoffError = errorOf(
          issues,
          'knowledgeCutoff',
          t('common.channels.profileWizard.knowledgeCutoffInvalid'),
        )
        const parameterError = errorOf(
          issues,
          'parameters',
          t('common.channels.profileWizard.paramInvalid'),
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
                  aria-controls={panelId}
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

            {locked ? (
              <p className="border-t border-border-hairline px-3 py-2 text-meta text-ink-3">
                {t('common.channels.profileWizard.lockedHint')}
              </p>
            ) : null}

            {open ? (
              <div
                id={panelId}
                className="grid gap-3 border-t border-border-hairline px-3 py-3 sm:grid-cols-2"
              >
                <TextField
                  label={t('common.channels.profileWizard.displayName')}
                  value={row.displayName}
                  error={displayError}
                  onChange={(event) => {
                    onUpdateRow(row.id, { displayName: event.target.value })
                  }}
                />
                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor={`profile-wizard-description-${row.id}`}
                    className={FIELD_LABEL_CLASSES}
                  >
                    {t('admin.models.field_description')}
                  </label>
                  <textarea
                    id={`profile-wizard-description-${row.id}`}
                    value={row.description}
                    aria-invalid={descriptionError === undefined ? undefined : true}
                    onChange={(event) => {
                      onUpdateRow(row.id, { description: event.target.value })
                    }}
                    className="min-h-20 w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  />
                  {descriptionError === undefined ? null : (
                    <p className="text-meta text-danger">{descriptionError}</p>
                  )}
                </div>

                <TextField
                  label={t('common.channels.profileWizard.context')}
                  inputMode="numeric"
                  value={row.contextLength}
                  error={contextError}
                  onChange={(event) => {
                    onUpdateRow(row.id, { contextLength: event.target.value })
                  }}
                />
                <TextField
                  label={t('common.channels.profileWizard.maxOutput')}
                  inputMode="numeric"
                  value={row.maxOutput}
                  error={maxOutputError}
                  onChange={(event) => {
                    onUpdateRow(row.id, { maxOutput: event.target.value })
                  }}
                />
                <TextField
                  label={t('admin.models.field_released_at')}
                  type="date"
                  value={row.releasedAt}
                  onChange={(event) => {
                    onUpdateRow(row.id, { releasedAt: event.target.value })
                  }}
                />

                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor={`profile-wizard-vendor-${row.id}`}
                    className={FIELD_LABEL_CLASSES}
                  >
                    {t('common.channels.profileWizard.vendor')}
                  </label>
                  <select
                    id={`profile-wizard-vendor-${row.id}`}
                    value={vendorChoice}
                    onChange={(event) => {
                      changeVendor(row, event.target.value)
                    }}
                    className={INPUT_CLASSES}
                  >
                    <option value="">{t('common.channels.form.none')}</option>
                    {vendorNames.length === 0 ? null : (
                      <optgroup
                        label={t(
                          'common.channels.profileWizard.vendorExisting',
                        )}
                      >
                        {vendorNames.map((name) => (
                          <option key={name} value={name}>
                            {name}
                          </option>
                        ))}
                      </optgroup>
                    )}
                    <option value={CREATE_VENDOR}>
                      {t('common.channels.profileWizard.vendorCreate')}
                    </option>
                  </select>
                  {vendorChoice === CREATE_VENDOR ? (
                    <TextField
                      label={t('common.channels.profileWizard.newVendorName')}
                      value={row.vendorName}
                      onChange={(event) => {
                        onUpdateRow(row.id, {
                          vendorName: event.target.value,
                        })
                      }}
                    />
                  ) : null}
                </div>

                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor={`profile-wizard-cutoff-${row.id}`}
                    className={FIELD_LABEL_CLASSES}
                  >
                    {t('common.channels.profileWizard.knowledgeCutoff')}
                  </label>
                  <input
                    id={`profile-wizard-cutoff-${row.id}`}
                    type="month"
                    value={row.knowledgeCutoff}
                    aria-invalid={cutoffError === undefined ? undefined : true}
                    onChange={(event) => {
                      onUpdateRow(row.id, {
                        knowledgeCutoff: event.target.value,
                      })
                    }}
                    className={INPUT_CLASSES}
                  />
                  {cutoffError === undefined ? null : (
                    <p className="text-meta text-danger">{cutoffError}</p>
                  )}
                </div>

                <div className="sm:col-span-2">
                  <TextField
                    label={t('common.channels.profileWizard.tags')}
                    value={tagDrafts[row.id] ?? row.tags.join(', ')}
                    onChange={(event) => {
                      const text = event.target.value
                      setTagDrafts((current) => ({
                        ...current,
                        [row.id]: text,
                      }))
                      onUpdateRow(row.id, { tags: parseTags(text) })
                    }}
                  />
                </div>

                <fieldset className="flex flex-col gap-2 sm:col-span-2">
                  <legend className={FIELD_LABEL_CLASSES}>
                    {t('common.channels.profileWizard.endpoints')}
                  </legend>
                  <div className="flex flex-wrap gap-1.5">
                    {ENDPOINT_KEYS.map((key: EndpointKey) => (
                      <ChipToggle
                        key={key}
                        checked={row.endpoints.includes(key)}
                        label={key}
                        onToggle={() => {
                          onUpdateRow(row.id, {
                            endpoints: toggled(row.endpoints, key),
                          })
                        }}
                      />
                    ))}
                  </div>
                </fieldset>

                <fieldset className="flex flex-col gap-2">
                  <legend className={FIELD_LABEL_CLASSES}>
                    {t('common.channels.profileWizard.capabilities')}
                  </legend>
                  <div className="flex flex-wrap gap-1.5">
                    {CAPABILITY_KEYS.map((key) => (
                      <ChipToggle
                        key={key}
                        checked={row.capabilities.includes(key)}
                        label={t(`admin.models.capability_${key}`)}
                        onToggle={() => {
                          onUpdateRow(row.id, {
                            capabilities: toggled(row.capabilities, key),
                          })
                        }}
                      />
                    ))}
                  </div>
                </fieldset>

                <fieldset className="flex flex-col gap-2">
                  <legend className={FIELD_LABEL_CLASSES}>
                    {t('common.channels.profileWizard.modalities')}
                  </legend>
                  <div className="flex flex-wrap gap-1.5">
                    {MODALITY_KEYS.map((key) => (
                      <ChipToggle
                        key={key}
                        checked={row.modalities.includes(key)}
                        label={t(`admin.models.modality_${key}`)}
                        onToggle={() => {
                          onUpdateRow(row.id, {
                            modalities: toggled(row.modalities, key),
                          })
                        }}
                      />
                    ))}
                  </div>
                </fieldset>

                <fieldset className="flex flex-col gap-2 sm:col-span-2">
                  <legend className={FIELD_LABEL_CLASSES}>
                    {t('common.channels.profileWizard.parameters')}
                  </legend>
                  {row.parameters.length === 0 ? (
                    <p className="text-meta text-ink-3">
                      {t('common.channels.profileWizard.parametersHint')}
                    </p>
                  ) : (
                    <ul className="flex flex-col gap-2">
                      {row.parameters.map((parameter, index) => (
                        <li
                          key={index}
                          className="flex flex-wrap items-end gap-2"
                        >
                          <input
                            aria-label={`${t('admin.models.param_name')} ${index + 1}`}
                            value={parameter.name}
                            onChange={(event) => {
                              updateParameter(row, index, {
                                name: event.target.value,
                              })
                            }}
                            className={`${INPUT_CLASSES} max-w-40`}
                          />
                          <input
                            aria-label={`${t('admin.models.param_type')} ${index + 1}`}
                            value={parameter.type}
                            aria-invalid={
                              parameterError === undefined ? undefined : true
                            }
                            onChange={(event) => {
                              updateParameter(row, index, {
                                type: event.target.value,
                              })
                            }}
                            className={`${INPUT_CLASSES} max-w-32`}
                          />
                          <input
                            aria-label={`${t('admin.models.param_description')} ${index + 1}`}
                            value={parameter.description}
                            onChange={(event) => {
                              updateParameter(row, index, {
                                description: event.target.value,
                              })
                            }}
                            className={`${INPUT_CLASSES} max-w-60`}
                          />
                          <Button
                            variant="ghost"
                            aria-label={`${t('common.channels.profileWizard.paramRemove')} ${index + 1}`}
                            onClick={() => {
                              removeParameter(row, index)
                            }}
                          >
                            ×
                          </Button>
                        </li>
                      ))}
                    </ul>
                  )}
                  <div className="flex flex-wrap items-center gap-2">
                    <Button
                      variant="secondary"
                      onClick={() => {
                        addParameter(row)
                      }}
                    >
                      {t('common.channels.profileWizard.paramAdd')}
                    </Button>
                    <span className="text-meta text-ink-3">
                      {t('common.channels.profileWizard.parametersHint')}
                    </span>
                  </div>
                  {parameterError === undefined ? null : (
                    <p className="text-meta text-danger">{parameterError}</p>
                  )}
                </fieldset>
              </div>
            ) : null}
          </div>
        )
      })}
    </section>
  )
}
