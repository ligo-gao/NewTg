import { useEffect, useState } from 'react'

/**
 * 离线状态（UI §6.12 六态「离线」）：离线时三个写按钮禁用、保留上次统计并
 * 标注「离线快照」。`navigator.onLine` 只是提示，服务端仍可能因网络路径
 * 失败返回 `ApiError.kind === 'offline'`，两者互不替代。
 *
 * 与插件页的同名 hook 保持一份小实现，避免 feature 之间横向耦合。
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
