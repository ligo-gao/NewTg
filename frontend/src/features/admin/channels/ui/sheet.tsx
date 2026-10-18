import type { ReactNode } from 'react'

import { Dialog } from '@/shared/ui/base/dialog'

export interface SheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
}

/**
 * Right-side sheet (modules-06 §3.1) built on the M0 Dialog primitive: the
 * brief requires a minimal Sheet without adding a dependency, so modal
 * semantics (focus trap, Escape, inert background) stay in one place.
 */
export function Sheet({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: SheetProps) {
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      description={description}
      footer={footer}
      variant="sheet"
    >
      {children}
    </Dialog>
  )
}
