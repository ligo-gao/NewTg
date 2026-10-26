/**
 * 向导第 2 步：批量默认值表单。
 *
 * 表单状态由容器持有（套用默认值时要与勾选行一起计算），本组件只负责
 * 渲染与局部切换。供应商下拉的 value 是供应商 id；写入体需要的名字在
 * 容器里通过 `VendorDirectory.nameById` 还原。
 */
import type { Dispatch, SetStateAction } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { TextField } from '@/shared/ui/form'

import {
  CAPABILITY_KEYS,
  ENDPOINT_KEYS,
  MODALITY_KEYS,
  type EndpointKey,
  type WizardDefaults,
} from './model-profile-wizard-logic'
import type { VendorDirectory } from './model-profile-wizard-queries'
import { ChipToggle } from './ui/chip-toggle'
import { toggled } from './ui/toggled'

/** 「本地没有，提交时新建」在下拉里的哨兵值（不会与真实供应商 id 冲突）。 */
export const CREATE_VENDOR = '__create__'

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'

export interface DefaultsFormState {
  defaults: WizardDefaults
  parameterText: string
  tagsText: string
  emptyOnly: boolean
  vendorChoice: string
  newVendorName: string
}

export function emptyDefaultsForm(): DefaultsFormState {
  return {
    defaults: {
      vendorName: '',
      endpoints: [],
      capabilities: [],
      modalities: [],
      parameters: [],
      knowledgeCutoff: '',
      tags: [],
    },
    parameterText: '',
    tagsText: '',
    emptyOnly: true,
    vendorChoice: '',
    newVendorName: '',
  }
}

export interface StepDefaultsProps {
  value: DefaultsFormState
  onChange: Dispatch<SetStateAction<DefaultsFormState>>
  vendorDirectory: VendorDirectory
  selectedCount: number
  appliedCount: number | null
  onApply: () => void
}

export function StepDefaults({
  value,
  onChange,
  vendorDirectory,
  selectedCount,
  appliedCount,
  onApply,
}: StepDefaultsProps) {
  const { t } = useI18n()
  const { defaults } = value

  function toggleEndpoint(key: EndpointKey): void {
    onChange((current) => ({
      ...current,
      defaults: {
        ...current.defaults,
        endpoints: toggled(current.defaults.endpoints, key),
      },
    }))
  }

  function toggleCapability(key: string): void {
    onChange((current) => ({
      ...current,
      defaults: {
        ...current.defaults,
        capabilities: toggled(current.defaults.capabilities, key),
      },
    }))
  }

  function toggleModality(key: string): void {
    onChange((current) => ({
      ...current,
      defaults: {
        ...current.defaults,
        modalities: toggled(current.defaults.modalities, key),
      },
    }))
  }

  return (
    <section className="flex flex-col gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5 sm:col-span-2">
          <label
            htmlFor="profile-wizard-vendor"
            className="text-meta text-ink-2"
          >
            {t('common.channels.profileWizard.vendor')}
          </label>
          <select
            id="profile-wizard-vendor"
            value={value.vendorChoice}
            onChange={(event) =>
              onChange((current) => ({
                ...current,
                vendorChoice: event.target.value,
              }))
            }
            className={INPUT_CLASSES}
          >
            <option value="">{t('common.channels.form.none')}</option>
            {vendorDirectory.names.length === 0 ? null : (
              <optgroup
                label={t('common.channels.profileWizard.vendorExisting')}
              >
                {vendorDirectory.names.map((name) => {
                  const id = vendorDirectory.ids.get(name.toLowerCase())
                  if (id === undefined) return null
                  return (
                    <option key={id} value={String(id)}>
                      {name}
                    </option>
                  )
                })}
              </optgroup>
            )}
            <option value={CREATE_VENDOR}>
              {t('common.channels.profileWizard.vendorCreate')}
            </option>
          </select>
          {value.vendorChoice === CREATE_VENDOR ? (
            <TextField
              label={t('common.channels.profileWizard.newVendorName')}
              value={value.newVendorName}
              onChange={(event) =>
                onChange((current) => ({
                  ...current,
                  newVendorName: event.target.value,
                }))
              }
            />
          ) : null}
        </div>

        <fieldset className="flex flex-col gap-2">
          <legend className="text-meta text-ink-2">
            {t('common.channels.profileWizard.endpoints')}
          </legend>
          <div className="flex flex-wrap gap-1.5">
            {ENDPOINT_KEYS.map((key) => (
              <ChipToggle
                key={key}
                checked={defaults.endpoints.includes(key)}
                label={key}
                onToggle={() => toggleEndpoint(key)}
              />
            ))}
          </div>
        </fieldset>

        <fieldset className="flex flex-col gap-2">
          <legend className="text-meta text-ink-2">
            {t('common.channels.profileWizard.capabilities')}
          </legend>
          <div className="flex flex-wrap gap-1.5">
            {CAPABILITY_KEYS.map((key) => (
              <ChipToggle
                key={key}
                checked={defaults.capabilities.includes(key)}
                label={t(`admin.models.capability_${key}`)}
                onToggle={() => toggleCapability(key)}
              />
            ))}
          </div>
        </fieldset>

        <fieldset className="flex flex-col gap-2">
          <legend className="text-meta text-ink-2">
            {t('common.channels.profileWizard.modalities')}
          </legend>
          <div className="flex flex-wrap gap-1.5">
            {MODALITY_KEYS.map((key) => (
              <ChipToggle
                key={key}
                checked={defaults.modalities.includes(key)}
                label={t(`admin.models.modality_${key}`)}
                onToggle={() => toggleModality(key)}
              />
            ))}
          </div>
        </fieldset>

        <div className="flex flex-col gap-1.5">
          <label
            htmlFor="profile-wizard-cutoff"
            className="text-meta text-ink-2"
          >
            {t('common.channels.profileWizard.knowledgeCutoff')}
          </label>
          <input
            id="profile-wizard-cutoff"
            type="month"
            value={defaults.knowledgeCutoff}
            onChange={(event) =>
              onChange((current) => ({
                ...current,
                defaults: {
                  ...current.defaults,
                  knowledgeCutoff: event.target.value,
                },
              }))
            }
            className={INPUT_CLASSES}
          />
        </div>

        <div className="sm:col-span-2">
          <TextField
            label={t('common.channels.profileWizard.tags')}
            value={value.tagsText}
            onChange={(event) =>
              onChange((current) => ({
                ...current,
                tagsText: event.target.value,
              }))
            }
          />
        </div>

        <div className="flex flex-col gap-1.5 sm:col-span-2">
          <label
            htmlFor="profile-wizard-parameters"
            className="text-meta text-ink-2"
          >
            {t('common.channels.profileWizard.parameters')}
          </label>
          <textarea
            id="profile-wizard-parameters"
            value={value.parameterText}
            onChange={(event) =>
              onChange((current) => ({
                ...current,
                parameterText: event.target.value,
              }))
            }
            placeholder={t('common.channels.profileWizard.parametersHint')}
            className="min-h-20 w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 font-mono text-body text-ink-1 placeholder:font-sans placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          />
          <p className="text-meta text-ink-3">
            {t('common.channels.profileWizard.parametersHint')}
          </p>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3 border-t border-border-hairline pt-3">
        <label className="flex items-center gap-2 text-meta text-ink-1">
          <input
            type="checkbox"
            checked={value.emptyOnly}
            onChange={(event) =>
              onChange((current) => ({
                ...current,
                emptyOnly: event.target.checked,
              }))
            }
            className="size-4 rounded border-border-default"
          />
          {t('common.channels.profileWizard.emptyOnly')}
        </label>
        <Button variant="secondary" onClick={onApply} disabled={selectedCount === 0}>
          {t('common.channels.profileWizard.applyDefaults')}
        </Button>
        {appliedCount === null ? null : (
          <span aria-live="polite" className="text-meta text-ink-3">
            {t('common.channels.profileWizard.defaultsApplied', {
              count: appliedCount,
            })}
          </span>
        )}
      </div>
    </section>
  )
}
