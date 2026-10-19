import { ApiError } from '@/api/errors'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

/**
 * 插件页统一的通知出口。错误 Toast 只用于「可重试的瞬态失败」；上传、
 * 编译与沙箱错误必须就地展示可复制文本（§6.10.1），因此那些路径直接渲染
 * `FormAlert`，不走这里。
 */
export function notifyError(error: unknown): void {
  if (error instanceof ApiError) {
    pushErrorToast(error)
    return
  }
  useToastStore.getState().push({
    tone: 'error',
    message: error instanceof Error ? error.message : String(error),
  })
}

export function notifySuccess(message: string): void {
  useToastStore.getState().push({ tone: 'success', message })
}
