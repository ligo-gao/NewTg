/**
 * 定价页的小型表单控件。
 *
 * 分段控件与开关都使用真实可聚焦元素，而不是给 `<div>` 绑点击：
 * 键盘用户要能 Tab 到达、用方向键/空格切换（modules-09 UI §0.6）。
 * 方向键行为复用 `shared/lib/roving-focus`，与仪表盘/日志的范围切换
 * 保持完全一致的手感。
 */

import type { ReactNode } from 'react'

import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'

export interface SegmentOption<T extends string> {
  value: T
  label: string
  disabled?: boolean
}

export interface SegmentedControlProps<T extends string> {
  label: string
  value: T
  options: readonly SegmentOption<T>[]
  onChange: (value: T) => void
  className?: string
}

export function SegmentedControl<T extends string>({
  label,
  value,
  options,
  onChange,
  className,
}: SegmentedControlProps<T>) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      onKeyDown={handleRadioGroupKeyDown}
      className={[
        'inline-flex items-center gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-1',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
    >
      {options.map((option) => {
        const selected = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={selected}
            aria-disabled={option.disabled === true ? true : undefined}
            disabled={option.disabled === true}
            tabIndex={selected ? 0 : -1}
            onClick={() => {
              if (option.disabled !== true) onChange(option.value)
            }}
            className={[
              'rounded-md px-2.5 py-0.5 text-[13px] font-medium leading-5 transition-colors duration-150 motion-reduce:transition-none',
              selected
                ? 'bg-surface-card text-ink-1 shadow-sm'
                : 'text-ink-2 hover:text-ink-1',
              option.disabled === true ? 'cursor-not-allowed opacity-50' : '',
              'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
            ]
              .filter(Boolean)
              .join(' ')}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

export interface SwitchControlProps {
  label: string
  checked: boolean
  disabled?: boolean
  disabledHint?: string
  onChange: (checked: boolean) => void
  children?: ReactNode
}

/**
 * 真实 `button[role="switch"]`。开关文字始终可见，`aria-label` 与可见文本
 * 一致，避免屏幕阅读器读出两个不同称谓。
 */
export function SwitchControl({
  label,
  checked,
  disabled = false,
  disabledHint,
  onChange,
  children,
}: SwitchControlProps) {
  return (
    <label
      className={[
        'inline-flex items-center gap-2 text-[13px] leading-5 text-ink-2',
        disabled ? 'cursor-not-allowed opacity-50' : 'cursor-pointer',
      ].join(' ')}
      title={disabled ? disabledHint : undefined}
    >
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        disabled={disabled}
        onClick={() => onChange(!checked)}
        className={[
          'relative inline-flex h-[22px] w-10 shrink-0 items-center rounded-full border transition-colors duration-150 motion-reduce:transition-none',
          checked ? 'border-brand bg-brand' : 'border-border-default bg-surface-card',
          'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
        ].join(' ')}
      >
        <span
          aria-hidden="true"
          className={[
            'inline-block size-4 rounded-full bg-surface-card shadow-sm transition-transform duration-150 motion-reduce:transition-none',
            checked ? 'translate-x-[20px]' : 'translate-x-[2px]',
          ].join(' ')}
        />
      </button>
      <span>{children ?? label}</span>
    </label>
  )
}
