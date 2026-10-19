import { useEffect, useState } from 'react'

/**
 * 离线状态（UI §6.9 六态「离线」）：离线时写按钮禁用并旁注原因，
 * 列表保留当前数据（React Query 不会因离线自动清空已缓存的行）。
 *
 * `navigator.onLine` 是提示而非保证：服务端仍可能因网络路径失败返回
 * `ApiError.kind === 'offline'`，两者互不替代。
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
