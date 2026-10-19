import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

/**
 * 部署状态六态（UI §6.13）：running / completed / failed /
 * deployment requested / termination requested / destroyed。
 *
 * 未知状态原样显示为中性徽章，绝不隐藏——provider 新增状态时管理员仍要
 * 能看到真实值，而不是空白单元格。
 */
export interface DeploymentStatusMeta {
  key: string
  variant: StatusBadgeVariant
}

const KNOWN_STATUSES: Record<string, DeploymentStatusMeta> = {
  running: { key: 'admin.deployments.status.running', variant: 'success' },
  completed: { key: 'admin.deployments.status.completed', variant: 'neutral' },
  failed: { key: 'admin.deployments.status.failed', variant: 'danger' },
  'deployment requested': {
    key: 'admin.deployments.status.deployment_requested',
    variant: 'info',
  },
  'termination requested': {
    key: 'admin.deployments.status.termination_requested',
    variant: 'warning',
  },
  destroyed: { key: 'admin.deployments.status.destroyed', variant: 'neutral' },
}

/** 状态筛选下拉的稳定顺序（与六态一致）。 */
export const DEPLOYMENT_STATUSES = Object.keys(KNOWN_STATUSES)

export function statusMeta(status: string): DeploymentStatusMeta | null {
  const normalized = status.trim().toLowerCase()
  return KNOWN_STATUSES[normalized] ?? null
}

export function DeploymentStatusBadge({ status }: { status: string }) {
  const { t } = useI18n()
  const meta = statusMeta(status)
  return (
    <StatusBadge variant={meta?.variant ?? 'neutral'} className="font-mono">
      {meta === null ? status : t(meta.key)}
    </StatusBadge>
  )
}
