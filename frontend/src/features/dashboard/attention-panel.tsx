import { useI18n } from '@/shared/lib/i18n-context'

export interface AttentionItem {
  id: string
  severity: 'warn' | 'info'
  title: string
  detail: string
  href?: string
  action?: string
}

export interface AttentionPanelProps {
  items: AttentionItem[]
}

/**
 * “需要处理”面板：白卡片 + 警戒色左边框。只放真实数据推导出的待办，
 * 没有待办时显示空态而不是伪造条目。
 */
export function AttentionPanel({ items }: AttentionPanelProps) {
  const { t } = useI18n()

  return (
    <section
      aria-label={t('common.dashboard.overview.attention.title')}
      data-testid="attention-panel"
      className="rounded-xl border border-border-hairline border-l-[3px] border-l-warning bg-surface-card p-4"
    >
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-body font-semibold text-ink-1">
          {t('common.dashboard.overview.attention.title')}
        </h2>
        <span className="font-mono text-badge text-ink-3">{items.length}</span>
      </div>
      {items.length === 0 ? (
        <p className="mt-3 text-badge text-ink-3">
          {t('common.dashboard.overview.attention.empty')}
        </p>
      ) : (
        <ul className="mt-1 list-none p-0">
          {items.map((item) => (
            <li
              key={item.id}
              className="border-t border-border-hairline py-2.5 first:border-t-0 first:pt-2"
            >
              <p className="flex items-baseline gap-2 text-badge font-medium text-ink-1">
                <span
                  aria-hidden="true"
                  className={[
                    'size-[5px] shrink-0 rounded-full',
                    item.severity === 'warn' ? 'bg-warning' : 'bg-brand',
                  ].join(' ')}
                />
                {item.title}
              </p>
              <p className="mt-1 pl-3 text-badge leading-5 text-ink-2">
                {item.detail}
              </p>
              {item.href !== undefined ? (
                <a
                  href={item.href}
                  className="mt-1 inline-block pl-3 text-badge text-brand underline underline-offset-2 hover:text-brand-hover"
                >
                  {item.action ?? t('common.dashboard.overview.attention.view')}
                </a>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
