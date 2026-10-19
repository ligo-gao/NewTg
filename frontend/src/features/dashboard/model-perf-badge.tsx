import { useI18n } from '@/shared/lib/i18n-context'
import { Tag, type TagVariant } from '@/shared/ui/tag'

import type { ModelPerfBadge as PerfBadge } from './model-perf-api'

export interface ModelPerfBadgeProps {
  badge: PerfBadge
  /** 传入时徽章作为看板入口（点击带 model 参数回到性能视图）。 */
  onSelect?: () => void
  className?: string
}

const BADGE_VARIANTS: Record<PerfBadge, TagVariant> = {
  fast: 'success',
  healthy: 'success',
  slow: 'warning',
  degraded: 'warning',
  incident: 'danger',
  no_data: 'neutral',
}

/** 图标与文字双编码：色盲用户不依赖颜色也能区分档位（UI §8.6）。 */
const BADGE_ICONS: Record<PerfBadge, string> = {
  fast: '⚡',
  healthy: '●',
  slow: '◷',
  degraded: '▲',
  incident: '✕',
  no_data: '?',
}

/**
 * `model-perf-badge`（modules-05 UI §8.6）。
 *
 * 徽章只表达当前窗口的模型健康，绝不显示猜测值；`no_data` 用虚线中性
 * 描边而不是成功色。徽章可点击时是一个真实按钮，键盘与读屏都能激活。
 */
export function ModelPerfBadge({
  badge,
  onSelect,
  className,
}: ModelPerfBadgeProps) {
  const { t } = useI18n()
  const label = t(`common.dashboard.performance.badge.${badge}`)
  const variant = BADGE_VARIANTS[badge]
  const content = (
    <>
      <span aria-hidden="true" className="mr-1">
        {BADGE_ICONS[badge]}
      </span>
      {label}
    </>
  )

  const tag = (
    <Tag
      variant={variant}
      ariaLabel={label}
      className={[badge === 'no_data' ? 'border-dashed' : '', className]
        .filter(Boolean)
        .join(' ')}
    >
      {content}
    </Tag>
  )

  if (onSelect === undefined) return tag

  return (
    <button
      type="button"
      data-testid="model-perf-badge"
      onClick={onSelect}
      className="rounded-[6px] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      {tag}
    </button>
  )
}
