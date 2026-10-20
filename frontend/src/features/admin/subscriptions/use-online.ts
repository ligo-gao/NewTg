import { useEffect, useState } from 'react'

/**
 * 离线状态（modules-00 §4.2 六态「离线」）：离线时订阅管理页的所有写操作
 * 禁用，读请求保持可用。`navigator.onLine` 只是提示——服务端仍可能因网络
 * 路径失败返回 `ApiError.kind === 'offline'`，两者互不替代。
 *
 * 与插件/性能页的同名 hook 保持一份小实现，避免 feature 之间横向耦合。
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
