import type { ReactNode } from 'react'

import { I18nProvider } from './i18n'
import { QueryProvider } from './query'
import { ThemeProvider } from './theme'
import { ToastViewport } from '@/shared/ui/toast'

export interface AppProvidersProps {
  children: ReactNode
}

/**
 * Application-wide providers.
 *
 * The Query client owns all server state (web-00 §5); i18n is the seven-language
 * runtime from §7 (dictionaries load per domain, preference follows §7.3). The
 * theme provider resolves light/dark/system from the same `newtg-theme` key the
 * marketing site uses and writes `data-theme` on the document root. The toast
 * viewport is mounted once here so any layer can surface global feedback
 * (modules-00 §4.8).
 */
export function AppProviders({ children }: AppProvidersProps) {
  return (
    <QueryProvider>
      <ThemeProvider>
        {/*
          syncPreference：真实应用需要按 web-00 §7.3 的优先级同步服务端语言
          偏好，并从 /api/status 读取语言集合。该同步器依赖 React Query，
          因此只在 AppProviders 中启用；单独渲染 I18nProvider 的测试不会发请求。
        */}
        <I18nProvider syncPreference>
          {children}
          <ToastViewport />
        </I18nProvider>
      </ThemeProvider>
    </QueryProvider>
  )
}
