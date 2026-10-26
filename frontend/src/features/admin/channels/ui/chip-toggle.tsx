/** 可勾选的胶囊按钮：视觉上是 chip，语义上是 checkbox。 */
export interface ChipToggleProps {
  checked: boolean
  label: string
  onToggle: () => void
}

export function ChipToggle({ checked, label, onToggle }: ChipToggleProps) {
  return (
    <label
      className={[
        'inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-full border px-2.5 text-badge transition-colors focus-within:shadow-[var(--focus-ring)]',
        checked
          ? 'border-brand-line bg-brand-subtle font-medium text-brand'
          : 'border-border-default bg-surface-card text-ink-2 hover:bg-surface-hover',
      ].join(' ')}
    >
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        className="sr-only"
      />
      {label}
    </label>
  )
}
