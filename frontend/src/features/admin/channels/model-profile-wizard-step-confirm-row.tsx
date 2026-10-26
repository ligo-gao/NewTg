/**
 * 第 3 步展开后的单行字段编辑器。
 *
 * 只负责一行的输入与行内报错，状态全部来自容器的 `row`，每次改动通过
 * `onUpdateRow` 回写。供应商下拉**只列本地目录 + 「不设置」**：新建供应商
 * 的唯一入口是第 2 步，这里不再提供行级自由输入。
 *
 * 校验文案必须自己报：写入体遇到非法数值 / 类型会静默丢字段。
 */
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { TextField } from '@/shared/ui/form'

import {
  CAPABILITY_KEYS,
  DESCRIPTION_MAX,
  DISPLAY_NAME_MAX,
  ENDPOINT_KEYS,
  MODALITY_KEYS,
  rowIssues,
  vendorStateOf,
  type EndpointKey,
  type ParameterRow,
  type RowIssue,
  type WizardRow,
} from './model-profile-wizard-logic'
import type { VendorDirectory } from './model-profile-wizard-queries'
import { ChipToggle } from './ui/chip-toggle'
import { toggled } from './ui/toggled'

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'

const FIELD_LABEL_CLASSES = 'text-meta text-ink-2'

function parseTags(text: string): string[] {
  return text
    .split(',')
    .map((tag) => tag.trim())
    .filter((tag) => tag !== '')
}

export interface StepConfirmRowEditorProps {
  row: WizardRow
  vendorDirectory: VendorDirectory
  /** 第 2 步显式填写的新建供应商名（空串 = 没选「新建」）。 */
  newVendorName: string
  onUpdateRow: (id: string, patch: Partial<WizardRow>) => void
}

export function StepConfirmRowEditor({
  row,
  vendorDirectory,
  newVendorName,
  onUpdateRow,
}: StepConfirmRowEditorProps) {
  const { t } = useI18n()
  // 标签输入保留原始文本，否则每次按键都会因 split / join 吞掉刚输入的逗号。
  const [tagDraft, setTagDraft] = useState<string | null>(null)
  const issues = rowIssues(row)
  const vendorState = vendorStateOf(row.vendorName, vendorDirectory, newVendorName)

  function errorOf(issue: RowIssue, message: string): string | undefined {
    return issues.includes(issue) ? message : undefined
  }

  const displayError = errorOf(
    'displayName',
    t('common.channels.profileWizard.displayNameTooLong', {
      count: DISPLAY_NAME_MAX,
    }),
  )
  const descriptionError = errorOf(
    'description',
    t('common.channels.profileWizard.descriptionTooLong', {
      count: DESCRIPTION_MAX,
    }),
  )
  const contextError = errorOf(
    'contextLength',
    t('common.channels.profileWizard.nonNegativeInteger'),
  )
  const maxOutputError = errorOf(
    'maxOutput',
    t('common.channels.profileWizard.nonNegativeInteger'),
  )
  const cutoffError = errorOf(
    'knowledgeCutoff',
    t('common.channels.profileWizard.knowledgeCutoffInvalid'),
  )
  const parameterError = errorOf(
    'parameters',
    t('common.channels.profileWizard.paramInvalid'),
  )

  function updateParameter(index: number, patch: Partial<ParameterRow>): void {
    onUpdateRow(row.id, {
      parameters: row.parameters.map((parameter, current) =>
        current === index ? { ...parameter, ...patch } : parameter,
      ),
    })
  }

  function removeParameter(index: number): void {
    onUpdateRow(row.id, {
      parameters: row.parameters.filter((_, current) => current !== index),
    })
  }

  function addParameter(): void {
    onUpdateRow(row.id, {
      parameters: [
        ...row.parameters,
        // 类型留空：空行视为「尚未添加」，不应一出现就被判非法。
        { name: '', type: '', description: '', defaultValue: '' },
      ],
    })
  }

  return (
    <div
      id={`profile-wizard-row-${row.id}`}
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
          {t('common.channels.profileWizard.description')}
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
          // 只在目录命中时回读名称；建议值 / 待新建值没有对应 option，
          // 用空值显示「不设置」，下方说明它本次的去向。
          value={vendorState.kind === 'listed' ? vendorState.name : ''}
          onChange={(event) => {
            onUpdateRow(row.id, { vendorName: event.target.value })
          }}
          className={INPUT_CLASSES}
        >
          <option value="">{t('common.channels.form.none')}</option>
          {vendorDirectory.names.length === 0 ? null : (
            <optgroup
              label={t('common.channels.profileWizard.vendorExisting')}
            >
              {vendorDirectory.names.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </optgroup>
          )}
        </select>
        {vendorState.kind === 'new' ? (
          <p className="text-meta text-ink-3">
            {t('common.channels.profileWizard.vendorWillCreate', {
              name: vendorState.name,
            })}
          </p>
        ) : null}
        {vendorState.kind === 'unresolved' ? (
          <p className="text-meta text-warning">
            {t('common.channels.profileWizard.vendorSuggestionUnresolved', {
              name: vendorState.name,
            })}
          </p>
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
            onUpdateRow(row.id, { knowledgeCutoff: event.target.value })
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
          value={tagDraft ?? row.tags.join(', ')}
          onChange={(event) => {
            const text = event.target.value
            setTagDraft(text)
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
              <li key={index} className="flex flex-wrap items-end gap-2">
                <input
                  aria-label={`${t('admin.models.param_name')} ${index + 1}`}
                  value={parameter.name}
                  onChange={(event) => {
                    updateParameter(index, { name: event.target.value })
                  }}
                  className={`${INPUT_CLASSES} max-w-40`}
                />
                <input
                  aria-label={`${t('admin.models.param_type')} ${index + 1}`}
                  value={parameter.type}
                  aria-invalid={parameterError === undefined ? undefined : true}
                  onChange={(event) => {
                    updateParameter(index, { type: event.target.value })
                  }}
                  className={`${INPUT_CLASSES} max-w-32`}
                />
                <input
                  aria-label={`${t('admin.models.param_description')} ${index + 1}`}
                  value={parameter.description}
                  onChange={(event) => {
                    updateParameter(index, { description: event.target.value })
                  }}
                  className={`${INPUT_CLASSES} max-w-60`}
                />
                <Button
                  variant="ghost"
                  aria-label={`${t('common.channels.profileWizard.paramRemove')} ${index + 1}`}
                  onClick={() => {
                    removeParameter(index)
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
              addParameter()
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
  )
}
