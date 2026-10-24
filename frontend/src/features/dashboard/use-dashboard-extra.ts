import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { meQueryOptions } from '@/lib/session'
import { fetchSystemStatus } from '@/features/admin/system/system-status-page'

import type { UsageRange } from './api'
import { useModelPerf } from './use-model-perf'

export interface DashboardPermissions {
  isAdmin: boolean
  canLogsRead: boolean
  canPricingRead: boolean
  canSettingsManage: boolean
}

/** Resolves the capabilities the dashboard panels need. Server stays authoritative. */
export function useDashboardPermissions(): DashboardPermissions {
  const me = useQuery(meQueryOptions()).data
  const permissions = me?.permissions ?? []
  const has = (key: string) => permissions.includes('*') || permissions.includes(key)
  const isAdmin = me?.role === 'admin'
  return {
    isAdmin,
    canLogsRead: isAdmin && has('logs:read'),
    canPricingRead: isAdmin && has('pricing:read'),
    canSettingsManage: isAdmin && has('settings:manage'),
  }
}

export interface DashboardExtraParams {
  range: UsageRange
  permissions: DashboardPermissions
}

/**
 * 仪表台的次要查询。每个面板独立失败：一个 403/503 只隐藏对应面板，
 * 不影响核心指标。管理面查询只在权限允许时启用，避免无谓的 403 噪音。
 */
export function useDashboardExtra({ range, permissions }: DashboardExtraParams) {
  const byUser = useQuery({
    queryKey: ['dashboard', 'by-user', range] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-user', {
          params: { query: { range } },
        }),
      ),
    enabled: permissions.canLogsRead,
    retry: false,
  })

  const byGroup = useQuery({
    queryKey: ['dashboard', 'by-group', range] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-stats/by-group', {
          params: { query: { range } },
        }),
      ),
    enabled: permissions.canLogsRead,
    retry: false,
  })

  const failures = useQuery({
    queryKey: ['dashboard', 'failures'] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-logs', {
          params: { query: { p: 1, page_size: 5, status: 'error' } },
        }),
      ),
    enabled: permissions.canLogsRead,
    retry: false,
  })

  const recent = useQuery({
    queryKey: ['dashboard', 'recent'] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/usage-logs', {
          params: { query: { p: 1, page_size: 5 } },
        }),
      ),
    enabled: !permissions.canLogsRead,
    retry: false,
  })

  const candidates = useQuery({
    queryKey: ['dashboard', 'pricing-candidates'] as const,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/model-pricing/candidates', {
          params: { query: { limit: 200 } },
        }),
      ),
    enabled: permissions.canPricingRead,
    retry: false,
  })

  const system = useQuery({
    queryKey: ['dashboard', 'system-status'] as const,
    queryFn: ({ signal }) => fetchSystemStatus(signal),
    enabled: permissions.canSettingsManage,
    retry: false,
  })

  const wallet = useQuery({
    queryKey: ['dashboard', 'wallet-summary'] as const,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
    enabled: !permissions.canLogsRead,
    retry: false,
  })

  // 只用于「指标库是否可用」这一条待办；不为它渲染完整性能看板。
  const perf = useModelPerf({ range: '24h', enabled: permissions.canLogsRead })

  return { byUser, byGroup, failures, recent, candidates, system, wallet, perf }
}
