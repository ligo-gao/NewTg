import type { ReactNode } from 'react'

// 公开页专属样式随布局一起按需加载：登录/注册等 auth 路由不经过这里，
// 因此不会再下载 57KB 的 home.css。
import '@/styles/shell-fonts.css'
import '@/styles/home.css'

import { PublicFooter } from './public-footer'
import { PublicHeader } from './public-header'

export interface PublicLayoutProps {
  /** Current pathname; drives `aria-current` on the nav. */
  activePath?: string
  /**
   * Wrap the page body in the standard 1200px reading container. The homepage
   * passes `false` because its sections own full-bleed surfaces and spacing
   * (UI design §0.7); every other public page keeps the contained layout.
   */
  contained?: boolean
  children: ReactNode
}

/**
 * Public shell (F-09-34/37): sticky header, content, footer.
 *
 * The footer is part of this shell rather than of each page, which is also why
 * console routes cannot render it — they use the `_console` layout (F-09-37).
 * `<main id="main">` matches the root skip link.
 */
export function PublicLayout({ activePath, contained = true, children }: PublicLayoutProps) {
  return (
    <div className="public-shell flex min-h-screen flex-col">
      <PublicHeader activePath={activePath} />
      <main id="main" className="flex-1">
        {contained ? (
          // 导航栏底边 → 页面首个文字块 = 56px（--space-14），四个公开页
          // 只由这里提供；页头组件（.rank-head / .about-hero）不得再写
          // 自己的 padding-top，否则就是两段留白相加。
          // 下沿保持 40px，页脚前与页内区块的节奏不变。
          <div className="wrap pt-14 pb-10">{children}</div>
        ) : (
          children
        )}
      </main>
      <PublicFooter />
    </div>
  )
}
