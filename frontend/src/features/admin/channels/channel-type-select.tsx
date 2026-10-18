import type { ChangeEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  CHANNEL_TYPE_GROUP_ORDER,
  channelTypesInGroup,
  isChannelType,
} from './types'

export interface ChannelTypeSelectProps {
  id?: string
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}

/**
 * Grouped picker over the 59 named channel types (R-P20).
 *
 * `contract.ChannelTypes` excludes the empty sentinel, so this control has no
 * empty option: the create form defaults to `openai` instead of rendering a
 * placeholder that would be indistinguishable from the sentinel. An unknown
 * incoming value (should not happen) is normalised to the first type rather
 * than introducing an empty option.
 */
export function ChannelTypeSelect({
  id,
  value,
  onChange,
  disabled = false,
  'aria-describedby': ariaDescribedBy,
  'aria-invalid': ariaInvalid,
}: ChannelTypeSelectProps) {
  const { t } = useI18n()
  const selected = isChannelType(value) ? value : CHANNEL_TYPE_GROUP_ORDER.flatMap(
    (group) => channelTypesInGroup(group),
  )[0] ?? ''

  function handleChange(event: ChangeEvent<HTMLSelectElement>): void {
    onChange(event.target.value)
  }

  return (
    <select
      id={id}
      value={selected}
      onChange={handleChange}
      disabled={disabled}
      aria-describedby={ariaDescribedBy}
      aria-invalid={ariaInvalid}
      className="h-9 w-full rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
    >
      {CHANNEL_TYPE_GROUP_ORDER.map((group) => (
        <optgroup key={group} label={t(`common.channels.typeGroup.${group}`)}>
          {channelTypesInGroup(group).map((type) => (
            <option key={type} value={type}>
              {type}
            </option>
          ))}
        </optgroup>
      ))}
    </select>
  )
}
