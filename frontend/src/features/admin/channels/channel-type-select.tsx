import { useId, type ChangeEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  CHANNEL_TYPE_GROUP_ORDER,
  channelTypesInGroup,
  isChannelType,
  nativeChannelTypeInfo,
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
 *
 * Native-protocol types (relay-02 §2.5) additionally get a human label and a
 * one-line description. The raw slug alone does not tell an administrator that
 * `moonshot` is an Anthropic-Messages upstream or that `vertex_ai` reuses the
 * Gemini encoding, which is exactly the distinction that matters when the
 * channel is later reached through cross-protocol forwarding.
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
  const generatedId = useId()
  // The description is part of the control's accessible name graph, so it must
  // be referenced by aria-describedby rather than merely rendered nearby.
  // A caller-supplied error reference is preserved and the description is
  // appended, keeping both announcements alive.
  const controlId = id ?? generatedId
  const descriptionId = `${controlId}-native-description`
  const selected = isChannelType(value) ? value : CHANNEL_TYPE_GROUP_ORDER.flatMap(
    (group) => channelTypesInGroup(group),
  )[0] ?? ''
  const nativeInfo = nativeChannelTypeInfo(selected)
  const describedBy =
    nativeInfo === undefined
      ? ariaDescribedBy
      : [ariaDescribedBy, descriptionId].filter((entry) => entry !== undefined).join(' ')

  function handleChange(event: ChangeEvent<HTMLSelectElement>): void {
    onChange(event.target.value)
  }

  return (
    <div className="flex flex-col gap-1">
      <select
        id={id}
        value={selected}
        onChange={handleChange}
        disabled={disabled}
        aria-describedby={describedBy}
        aria-invalid={ariaInvalid}
        className="h-9 w-full rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      >
        {CHANNEL_TYPE_GROUP_ORDER.map((group) => (
          <optgroup key={group} label={t(`common.channels.typeGroup.${group}`)}>
            {channelTypesInGroup(group).map((type) => {
              const info = nativeChannelTypeInfo(type)
              return (
                <option key={type} value={type}>
                  {info === undefined ? type : t(info.labelKey)}
                </option>
              )
            })}
          </optgroup>
        ))}
      </select>
      {nativeInfo === undefined ? null : (
        <p id={descriptionId} className="text-xs text-ink-3">
          {t(nativeInfo.descriptionKey)}
        </p>
      )}
      {/* `nativeInfo` is derived from the selected type, so switching types
          updates the description in the same render as the select value. */}
    </div>
  )
}
