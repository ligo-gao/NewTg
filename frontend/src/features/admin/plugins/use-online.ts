import { useEffect, useState } from 'react'

/**
 * 离线状态（§6.10.1 六态「离线」）：离线时写按钮禁用并旁注原因，当前列表
 * 与详情保留只读展示。`navigator.onLine` 只是提示；服务端仍可能因网络路径
 * 失败返回 `ApiError.kind === 'offline'`，两者互不替代。
 *
 * 插件功能不依赖其它 admin 子域，因此保留一份小实现，避免横向耦合。
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
