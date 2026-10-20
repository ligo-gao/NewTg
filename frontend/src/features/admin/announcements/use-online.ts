import { useEffect, useState } from 'react'

/**
 * 离线状态（UI §6.1.1 六态「离线」）：离线时写操作禁用、列表保留当前快照，
 * 恢复后 React Query 会在重连时补一次重查。
 *
 * `navigator.onLine` 只是提示；传输层仍可能以 `ApiError.kind === 'offline'`
 * 表达失败，两者互不替代。
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
