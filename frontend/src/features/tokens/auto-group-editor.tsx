import { useId, useRef, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Tag } from '@/shared/ui/tag'

export interface AutoGroupCandidate {
  name: string
  ratio?: number
}

export interface AutoGroupEditorProps {
  candidates: AutoGroupCandidate[]
  maxCount: number
  value: string[]
  onChange: (next: string[]) => void
  disabled?: boolean
  error?: string
}

/**
 * Ordered auto-group editor (modules-02 §2.12, F-02-13/F-02-14).
 *
 * Order is semantic: earlier groups are tried first, so every item shows its
 * 1-based index and offers two non-drag paths (up/down buttons) in addition to
 * HTML5 drag-and-drop. The server owns the maximum entry count; we only render
 * it and disable "add" once reached.
 */
export function AutoGroupEditor({
  candidates,
  maxCount,
  value,
  onChange,
  disabled = false,
  error,
}: AutoGroupEditorProps) {
  const { t } = useI18n()
  const selectId = useId()
  const [pending, setPending] = useState('')
  const dragIndex = useRef<number | null>(null)

  const ratioOf = (name: string): number | undefined =>
    candidates.find((candidate) => candidate.name === name)?.ratio
  const remaining = candidates.filter((candidate) => !value.includes(candidate.name))
  const full = value.length >= maxCount

  function move(from: number, to: number): void {
    if (to < 0 || to >= value.length || from === to) return
    const next = [...value]
    const [item] = next.splice(from, 1)
    if (item === undefined) return
    next.splice(to, 0, item)
    onChange(next)
  }

  function add(): void {
    if (pending === '' || full || value.includes(pending)) return
    onChange([...value, pending])
    setPending('')
  }

  return (
    <div className="rounded-lg border border-border-default bg-surface-subtle p-3">
      <p className="text-[13px] font-medium text-ink-1">{t('common.tokens.autoGroups.title')}</p>
      <p className="mt-0.5 text-xs text-ink-3">{t('common.tokens.autoGroups.hint')}</p>

      {candidates.length === 0 ? (
        <p role="alert" className="mt-2 text-xs text-warning">
          {t('common.tokens.autoGroups.empty')}
        </p>
      ) : null}

      <ol className="mt-2 flex flex-col gap-1">
        {value.map((name, index) => (
          <li
            key={name}
            data-group={name}
            data-testid="auto-group-item"
            draggable={!disabled}
            onDragStart={() => {
              dragIndex.current = index
            }}
            onDragOver={(event) => {
              event.preventDefault()
            }}
            onDrop={() => {
              const from = dragIndex.current
              dragIndex.current = null
              if (from !== null) move(from, index)
            }}
            className="flex h-11 items-center gap-2 rounded-md border border-border-hairline bg-surface-card px-2"
          >
            <span
              aria-hidden="true"
              className="cursor-grab text-ink-3"
              title={t('common.tokens.autoGroups.dragHandle', { name })}
            >
              ⠿
            </span>
            <span className="w-6 text-right text-xs tabular-nums text-ink-3">
              {index + 1}
            </span>
            <Tag>{name}</Tag>
            {ratioOf(name) !== undefined ? (
              <span className="text-xs tabular-nums text-warning">×{ratioOf(name)}</span>
            ) : null}
            <span className="ml-auto flex items-center gap-1">
              <button
                type="button"
                disabled={disabled || index === 0}
                aria-label={t('common.tokens.autoGroups.moveUp', { name })}
                onClick={() => {
                  move(index, index - 1)
                }}
                className="px-1 text-xs text-ink-2 disabled:opacity-40"
              >
                ↑
              </button>
              <button
                type="button"
                disabled={disabled || index === value.length - 1}
                aria-label={t('common.tokens.autoGroups.moveDown', { name })}
                onClick={() => {
                  move(index, index + 1)
                }}
                className="px-1 text-xs text-ink-2 disabled:opacity-40"
              >
                ↓
              </button>
              <button
                type="button"
                disabled={disabled}
                aria-label={t('common.tokens.autoGroups.remove', { name })}
                onClick={() => {
                  onChange(value.filter((entry) => entry !== name))
                }}
                className="px-1 text-xs text-danger disabled:opacity-40"
              >
                ×
              </button>
            </span>
          </li>
        ))}
      </ol>

      <div className="mt-2 flex items-end gap-2">
        <div className="flex flex-col gap-1">
          <label htmlFor={selectId} className="text-xs text-ink-2">
            {t('common.tokens.autoGroups.add')}
          </label>
          <select
            id={selectId}
            value={pending}
            disabled={disabled || full || remaining.length === 0}
            onChange={(event) => {
              setPending(event.target.value)
            }}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 disabled:opacity-50"
          >
            <option value="">
              {full
                ? t('common.tokens.autoGroups.max', { max: maxCount })
                : t('common.tokens.autoGroups.add')}
            </option>
            {remaining.map((candidate) => (
              <option key={candidate.name} value={candidate.name}>
                {candidate.name}
              </option>
            ))}
          </select>
        </div>
        <button
          type="button"
          disabled={disabled || full || pending === ''}
          onClick={add}
          className="h-9 rounded-lg border border-border-default px-3 text-sm text-ink-1 disabled:opacity-40"
        >
          {t('common.tokens.autoGroups.addAction')}
        </button>
        <span className="ml-auto text-xs tabular-nums text-ink-2">
          {t('common.tokens.autoGroups.selected', {
            count: value.length,
            max: maxCount,
          })}
        </span>
      </div>
      {error !== undefined ? <p className="mt-1 text-xs text-danger">{error}</p> : null}
    </div>
  )
}
