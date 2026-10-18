import { Component, type ErrorInfo, type ReactNode } from 'react'

export interface ErrorFallbackProps {
  error: unknown
  reset?: () => void
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : 'Unexpected error'
}

/** Shared visual fallback for render errors and route-level error components. */
export function ErrorFallback({ error, reset }: ErrorFallbackProps) {
  return (
    <main
      role="alert"
      className="flex min-h-screen flex-col items-center justify-center gap-3 bg-surface-page p-6 text-center text-ink-1"
    >
      <h1 className="text-xl font-semibold">Something went wrong</h1>
      <p className="max-w-prose text-sm text-ink-2">{messageOf(error)}</p>
      {reset ? (
        <button
          type="button"
          onClick={reset}
          className="mt-2 h-9 rounded-lg bg-brand px-3.5 text-sm font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          Try again
        </button>
      ) : null}
    </main>
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
