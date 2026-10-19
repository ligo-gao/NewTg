import { Component, type ErrorInfo, type ReactNode } from 'react'

import { ApiError } from '@/api/errors'

import { ServerErrorPage } from './error-status-page'

export interface ErrorFallbackProps {
  error: unknown
  reset?: () => void
}

/**
 * 从错误对象读取 `request_id`（web-00 §8.4）。只接受非空字符串，避免把
 * 异常对象上的任意字段塞进 UI；取不到时模板不渲染 request_id 区块。
 */
function requestIdOf(error: unknown): string | undefined {
  if (typeof error !== 'object' || error === null) return undefined
  const value = (error as { request_id?: unknown }).request_id
  return typeof value === 'string' && value.length > 0 ? value : undefined
}

/**
 * F-09-08 / F-09-12：渲染异常统一映射到 500 页，而不是显示原始错误消息
 * （原始 detail 可能含上游/内部信息，GC-2 禁止外露）。`ApiError.kind ===
 * 'offline'` 时由外壳换成离线文案，避免把断网误报成服务器故障。
 *
 * 该组件同时被全局 ErrorBoundary（位于 AppProviders 之外）与根路由的
 * `errorComponent` 使用，因此它的文案走 `useErrorPageTranslate` 的
 * provider 缺失降级路径。
 */
export function ErrorFallback({ error, reset }: ErrorFallbackProps) {
  const networkFailure = error instanceof ApiError && error.kind === 'offline'

  return (
    <ServerErrorPage
      requestId={requestIdOf(error)}
      networkFailure={networkFailure}
      retryable={reset !== undefined}
      onRetry={reset}
    />
  )
}

export interface ErrorBoundaryProps {
  children: ReactNode
  fallback?: (props: ErrorFallbackProps) => ReactNode
}

interface ErrorBoundaryState {
  error: unknown
}

/** Global render error boundary (web-00 §8). */
export class ErrorBoundary extends Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  state: ErrorBoundaryState = { error: null }

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { error }
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo): void {
    console.error('Unhandled render error', error, errorInfo.componentStack)
  }

  private readonly reset = (): void => {
    this.setState({ error: null })
  }

  render(): ReactNode {
    const { error } = this.state
    if (error === null || error === undefined) {
      return this.props.children
    }
    const { fallback } = this.props
    if (fallback) {
      return fallback({ error, reset: this.reset })
    }
    return <ErrorFallback error={error} reset={this.reset} />
  }
}
