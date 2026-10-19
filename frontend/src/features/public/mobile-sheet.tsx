import type { ReactNode } from 'react'

import { Dialog } from '@/shared/ui/base/dialog'

export interface MobileSheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  closeLabel: string
  children?: ReactNode
  footer?: ReactNode
}

/**
 * F-09-23 mobile filter sheet.
 *
 * No second modal implementation: this composes the shared Base UI
 * `Dialog` (variant `sheet`), which owns portal semantics, `aria-modal`, the
 * focus trap and focus restore (web-00 §2.1, §10.3). `Dialog` exposes
 * `panelClassName`, so the same element can stack as a full-width bottom sheet
 * below 768px and stay a right-hand sheet above it — the `Sheet` primitive does
 * not accept panel classes, and `shared/ui/base` is frozen for Task 5.
 *
 * The grab handle is decorative (`aria-hidden`); the drag gesture itself is
 * not required by the P1 acceptance criteria, and adding pointer handlers
 * without a keyboard equivalent would create a second, inaccessible dismiss
 * path.
 */
export function MobileSheet({
  open,
  onOpenChange,
  title,
  description,
  closeLabel,
  children,
  footer,
}: MobileSheetProps) {
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      description={description}
      variant="sheet"
      panelClassName="pr-10 max-md:mt-auto max-md:h-auto max-md:max-h-[85vh] max-md:max-w-none max-md:rounded-t-xl max-md:border-l-0 max-md:border-t max-md:border-border-hairline max-md:p-5 max-md:pr-10"
    >
      <div
        aria-hidden="true"
        data-testid="mobile-sheet-handle"
        className="absolute left-1/2 top-2 h-1 w-10 -translate-x-1/2 rounded-full bg-border-default md:hidden"
      />
      {children}
      {footer ? <div className="mt-5 flex justify-end gap-2">{footer}</div> : null}
      <button
        type="button"
        aria-label={closeLabel}
        onClick={() => onOpenChange(false)}
        className="absolute right-4 top-4 inline-flex size-8 items-center justify-center rounded-lg text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
          <path d="m4 4 8 8M12 4l-8 8" strokeLinecap="round" />
        </svg>
      </button>
    </Dialog>
  )
}
