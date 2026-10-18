import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { meQueryOptions } from '@/lib/session'

export interface PricingCapabilities {
  /** 操作者授权已知（或会话表明非 admin）后为 true。 */
  known: boolean
  /** 探测仍在进行；页面据此显示加载态而不是发注定失败的列表请求。 */
  loading: boolean
  canRead: boolean
  canManage: boolean
}

const FULL_ACCESS: PricingCapabilities = {
  known: true,
  loading: false,
  canRead: true,
  canManage: true,
}

/**
 * 解析当前登录管理员是否持有 pricing:read / pricing:manage。
 *
 * MeView 刻意不下发权限矩阵（R-17），所以沿用 M1 UserMutateSheet 的做法：
 * 按用户名精确匹配，从 GET /api/users 读调用者自己的行。admin_permissions
 * === null（FULL）或 ["*"] 表示全权；空数组表示无任何权限。
 *
 * 客户端只是 UX 闸门——每个定价端点都在服务端强制同一权限
 * （web-00 §9.1）。行仍在加载、或调用者连用户列表都不可读时，写操作
 * 保持隐藏（UI fail-closed），服务端仍是最终权威。
 */
export function usePricingCapabilities(): PricingCapabilities {
  const meQuery = useQuery(meQueryOptions())
  const username = meQuery.data?.username

  const actorQuery = useQuery({
    queryKey: ['admin-users', 'actor', username],
    // 非管理员没有权限矩阵可读，调用 /api/users 只会得到 403；控制台导航
    // 对每个页面都会挂载本 hook，因此必须在这里就短路。
    enabled: username !== undefined && meQuery.data?.role === 'admin',
    staleTime: 0,
    queryFn: async () => {
      const data = requireData(
        await apiClient.GET('/api/users', {
          params: { query: { keyword: username ?? '', p: 1, page_size: 20 } },
        }),
      )
      return (
        (data.items ?? []).find(
          (candidate) => candidate.username === username,
        ) ?? null
      )
    },
  })

  if (meQuery.isPending) {
    // 会话探测仍在进行：先渲染只读视图让表格能加载，但在授权已知前
    // 隐藏写操作。
    return { known: false, loading: true, canRead: true, canManage: false }
  }
  if (meQuery.data === undefined || meQuery.data.role !== 'admin') {
    return { known: true, loading: false, canRead: false, canManage: false }
  }
  if (actorQuery.isPending) {
    return { known: false, loading: true, canRead: true, canManage: false }
  }
  if (actorQuery.data === undefined) {
    // 用户列表不可读（例如没有 users:read）：能力无法判定。允许列表请求
    // 让服务端裁决读权限，但写操作保持隐藏（fail-closed）。
    return { known: false, loading: false, canRead: true, canManage: false }
  }
  if (actorQuery.data === null) {
    // 管理员自己的行都查不到：按无权限处理。
    return { known: true, loading: false, canRead: false, canManage: false }
  }

  const permissions = actorQuery.data.admin_permissions
  if (permissions === null || permissions.includes('*')) return FULL_ACCESS
  return {
    known: true,
    loading: false,
    canRead: permissions.includes('pricing:read'),
    canManage: permissions.includes('pricing:manage'),
  }
}
