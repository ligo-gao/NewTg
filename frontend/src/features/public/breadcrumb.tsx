import { useI18n } from '@/shared/lib/i18n-context'

export interface BreadcrumbItem {
  label: string
  /** 站内路径；末级或禁用层可省略。 */
  path?: string
  /** 末级（当前页）：不可点击并带 `aria-current="page"`。 */
  current?: boolean
  /** 目标页未开放时仍展示层级，但不可点击（modules-09 F-09-36 分支流程）。 */
  disabled?: boolean
  disabledHint?: string
}

export interface BreadcrumbProps {
  items: BreadcrumbItem[]
  /** 导航地标名称；默认取 i18n。 */
  label?: string
}

function ItemContent({ item }: { item: BreadcrumbItem }) {
  const isCurrent = item.current === true
  const className = isCurrent
    ? 'text-ink-2'
    : item.disabled === true
      ? 'cursor-not-allowed text-ink-3'
      : 'text-brand hover:underline'

  if (!isCurrent && item.disabled !== true && item.path !== undefined) {
    return (
      <a href={item.path} className={className}>
        {item.label}
      </a>
    )
  }

  return (
    <span
      aria-current={isCurrent ? 'page' : undefined}
      aria-disabled={item.disabled === true ? true : undefined}
      title={item.disabled === true ? item.disabledHint : undefined}
      className={className}
    >
      {item.label}
    </span>
  )
}

function Separator() {
  return (
    <span aria-hidden="true" className="text-ink-4">
      ›
    </span>
  )
}

/**
 * F-09-36 breadcrumb. Rendering rules are intentionally data-driven: the page
 * supplies the level list and the component only enforces the shared rules
 * (depth ≥ 2 to render, link head/middle, mark the tail as current).
 *
 * The >3-level fold uses native `<details>`: keyboard support, focus order and
 * screen-reader semantics come from the platform instead of a bespoke popover,
 * and the collapsed levels stay in the DOM when expanded.
 */
export function Breadcrumb({ items, label }: BreadcrumbProps) {
  const { t } = useI18n()
  if (items.length < 2) return null

  const head = items[0]
  const tail = items[items.length - 1]
  const middle = items.slice(1, -1)
  const fold = middle.length > 1

  return (
    <nav aria-label={label ?? t('public.breadcrumb.label')} className="text-[13px]">
      <ol className="flex flex-wrap items-center gap-2">
        <li>{head === undefined ? null : <ItemContent item={head} />}</li>
        <li aria-hidden="true">
          <Separator />
        </li>
        {fold ? (
          <li>
            <details className="relative inline-block">
              <summary
                aria-label={t('public.breadcrumb.more')}
                className="cursor-pointer list-none rounded px-1 text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                …
              </summary>
              <ul className="absolute left-0 top-full z-20 mt-1 min-w-[160px] rounded-lg border border-border-hairline bg-surface-card p-2 shadow-lg">
                {middle.map((item) => (
                  <li key={`${item.path ?? item.label}`} className="px-1 py-1">
                    <ItemContent item={item} />
                  </li>
                ))}
              </ul>
            </details>
          </li>
        ) : (
          middle.map((item) => (
            <li key={`${item.path ?? item.label}`} className="flex items-center gap-2">
              <ItemContent item={item} />
              <Separator />
            </li>
          ))
        )}
        {fold ? (
          <li aria-hidden="true">
            <Separator />
          </li>
        ) : null}
        <li>{tail === undefined ? null : <ItemContent item={{ ...tail, current: true }} />}</li>
      </ol>
    </nav>
  )
}
