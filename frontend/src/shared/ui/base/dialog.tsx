import { useEffect, useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

export interface DialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
}

/** Keyboard-focusable descendants, in document order (WAI-ARIA dialog pattern). */
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableInside(panel: HTMLElement): HTMLElement[] {
  return Array.from(
    panel.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR),
  ).filter(
    (element) =>
      element.tabIndex !== -1 &&
      element.getAttribute('aria-hidden') !== 'true' &&
      // `display:none` / `visibility:hidden` elements are not tabbable in a
      // browser; without this filter a hidden first/last candidate would
      // receive the wrap-around focus and freeze keyboard navigation.
      // (`getClientRects()` is empty when no box is generated.)
      element.getClientRects().length > 0,
  )
}

/**
 * Minimal Base UI Dialog primitive (web-00 §2.1, modules-00 §4.8).
 * M0 scope: portal, modal semantics, Escape/overlay close, focus trap and
 * focus restore. The background is made inert while the dialog is open so
 * neither pointer nor keyboard nor a screen reader can reach it.
 */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: DialogProps) {
  const panelRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descriptionId = useId()

  // Callers routinely pass an inline arrow for `onOpenChange`. Reading it
  // through a ref keeps the focus lifecycle effect keyed on `[open]` alone;
  // depending on the callback directly tore the effect down and re-ran focus
  // capture on every parent render, which produced a visible focus flicker.
  const onOpenChangeRef = useRef(onOpenChange)
  useEffect(() => {
    onOpenChangeRef.current = onOpenChange
  }, [onOpenChange])

  // One effect owns the whole focus lifecycle so capture, inertness and
  // restoration happen in a safe order:
  //   1. record the trigger before focus moves;
  //   2. move focus to the panel;
  //   3. mark background siblings inert;
  //   cleanup: undo inertness *before* restoring focus (an inert element
  //   cannot receive focus in browsers that implement `inert`).
  useEffect(() => {
    if (!open) return

    const panel = panelRef.current
    if (panel === null) return

    const previouslyFocused =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null

    panel.focus()

    const portalRoot = panel.parentElement
    const background = Array.from(document.body.children).filter(
      (element) => element !== portalRoot,
    )
    const snapshots = background.map((element) => ({
      element,
      wasInert: element.hasAttribute('inert'),
      ariaHidden: element.getAttribute('aria-hidden'),
    }))
    for (const snapshot of snapshots) {
      snapshot.element.setAttribute('inert', '')
      // `inert` alone is not honoured by every AT/webview combo; aria-hidden is
      // the belt-and-braces signal that the background is not part of the modal.
      snapshot.element.setAttribute('aria-hidden', 'true')
    }

    const handleKeyDown = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onOpenChangeRef.current(false)
        return
      }
      if (event.key !== 'Tab') return

      const focusable = focusableInside(panel)
      const first = focusable[0]
      const last = focusable[focusable.length - 1]

      if (first === undefined || last === undefined) {
        // Nothing focusable inside: keep Tab from escaping to the page.
        event.preventDefault()
        panel.focus()
        return
      }

      const active = document.activeElement
      const focusIsOutside = active === null || !panel.contains(active)

      if (event.shiftKey) {
        if (focusIsOutside || active === first || active === panel) {
          event.preventDefault()
          last.focus()
        }
        return
      }

      if (focusIsOutside || active === last || active === panel) {
        event.preventDefault()
        first.focus()
      }
    }

    document.addEventListener('keydown', handleKeyDown)

    return () => {
      document.removeEventListener('keydown', handleKeyDown)
      for (const snapshot of snapshots) {
        if (!snapshot.wasInert) snapshot.element.removeAttribute('inert')
        if (snapshot.ariaHidden === null) {
          snapshot.element.removeAttribute('aria-hidden')
        } else {
          snapshot.element.setAttribute('aria-hidden', snapshot.ariaHidden)
        }
      }
      // The trigger may have been unmounted while the dialog was open; focusing
      // a detached node is a no-op, so only restore a still-connected trigger.
      if (previouslyFocused?.isConnected) {
        previouslyFocused.focus()
      }
    }
  }, [open])

  if (!open) {
    return null
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-overlay"
        aria-hidden="true"
        onClick={() => {
          onOpenChange(false)
        }}
      />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        className="relative w-full max-w-[440px] rounded-xl border border-border-hairline bg-surface-card p-5 text-ink-1 shadow-xl"
      >
        <h2 id={titleId} className="text-[15px] font-semibold leading-[22px]">
          {title}
        </h2>
        {description ? (
          <p id={descriptionId} className="mt-1 text-sm text-ink-2">
            {description}
          </p>
        ) : null}
        {children ? <div className="mt-4 text-sm text-ink-2">{children}</div> : null}
        {footer ? <div className="mt-5 flex justify-end gap-2">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  )
}
