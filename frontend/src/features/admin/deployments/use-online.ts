import { useEffect, useState } from 'react'

/**
 * 离线状态（UI §6.13 六态「离线」）：离线时所有写按钮禁用并旁注原因，
 * 当前列表/详情保留只读。`navigator.onLine` 只是提示；服务端仍可能因网络
 * 路径失败返回 `ApiError.kind === 'offline'`，两者互不替代。
 *
 * 部署功能不依赖通知 feature，因此这里保留一份 12 行实现，避免 admin 子域
 * 之间横向耦合。
 */
export function useOnlineStatus(): boolean {
  const [online, setOnline] = useState(() =>
    typeof navigator === 'undefined' ? true : navigator.onLine,
  )

  useEffect(() => {
    const handleOnline = (): void => setOnline(true)
    const handleOffline = (): void => setOnline(false)
    window.addEventListener('online', handleOnline)
    window.addEventListener('offline', handleOffline)
    return () => {
      window.removeEventListener('online', handleOnline)
      window.removeEventListener('offline', handleOffline)
    }
  }, [])

  return online
}
