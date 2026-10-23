import type { ReactNode } from 'react'

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
          <div className="wrap py-10">{children}</div>
        ) : (
          children
        )}
      </main>
      <PublicFooter />
    </div>
  )
}
