import { useEffect, useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

export interface DialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
  /**
   * When false, Escape and backdrop clicks are ignored (e.g. a step that must
   * not be dismissed before the user saves one-time material). Defaults true.
   */
  dismissible?: boolean
  /**
   * `sheet` renders the same modal semantics as a right-side panel (used by the
   * wide channel form, modules-06 §3.1). Focus trapping, Escape handling and
   * background inertness are identical; only the frame changes.
   */
  variant?: 'center' | 'sheet'
  /** Extra classes for the panel; callers size sheets without owning the frame. */
  panelClassName?: string
  /**
   * `alertdialog` is required when the dialog blocks an unsaved/destructive
   * decision and demands an immediate response (WAI-ARIA). Defaults to the
   * regular `dialog` role used by the rest of the console.
   */
  role?: 'dialog' | 'alertdialog'
  /**
   * Where focus lands when the dialog opens. `panel` (default) keeps focus on
   * the container so a screen reader starts at the title; confirmations that
   * ask about a destructive/irreversible action pass `first` to land on the
   * least destructive control (the Cancel button, rendered first in the
   * footer) instead of the confirming one.
   */
  initialFocus?: 'panel' | 'first' | 'last'
  /**
   * Opt-in labelled close button in the title row (it calls `onOpenChange(false)`
   * and becomes the first focusable element, before `children`). Omitting it
   * leaves the DOM and focus order exactly as before; a non-dismissible dialog
   * never renders it.
   */
  closeLabel?: string
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
      // `display:none` generates no box at all, so `getClientRects()` is empty
      // and the element never accepts focus; without this filter a hidden
      // first/last candidate would receive the wrap-around focus and freeze
      // keyboard navigation. Note this is not a general "hidden" test:
      // `visibility:hidden` still generates boxes (non-empty rects) even
      // though browsers do not focus such elements.
      element.getClientRects().length > 0,
  )
}

/**
 * Module-level modal stack (WAI-ARIA nested dialogs).
 *
 * Every open `Dialog` pushes a token; only the last one — the visually topmost
 * portal — may consume Tab/Escape. Without this, a confirmation opened from a
 * drawer would leave the drawer's key handler alive: Escape would dismiss both
 * layers at once and Tab could jump into the inert background panel. Tokens
 * (not booleans) make removal order-independent when nested effects clean up.
 */
const dialogStack: symbol[] = []

function isTopDialog(token: symbol): boolean {
  return dialogStack[dialogStack.length - 1] === token
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
  dismissible = true,
  variant = 'center',
  panelClassName,
  role = 'dialog',
  initialFocus = 'panel',
  closeLabel,
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

  const dismissibleRef = useRef(dismissible)
  useEffect(() => {
    dismissibleRef.current = dismissible
  }, [dismissible])

  const initialFocusRef = useRef(initialFocus)
  useEffect(() => {
    initialFocusRef.current = initialFocus
  }, [initialFocus])

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

    // Register before the first paint of the handler; the token identifies
    // this specific dialog on the module-level stack.
    const token = Symbol('dialog')
    dialogStack.push(token)

    panel.focus()

    if (initialFocusRef.current !== 'panel') {
      const focusable = focusableInside(panel)
      const target =
        initialFocusRef.current === 'first'
          ? focusable[0]
          : focusable[focusable.length - 1]
      target?.focus()
    }

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
      // Only the topmost modal owns keyboard navigation. A lower dialog stays
      // mounted (and inert) while a confirmation is on top, but must not react
      // to Escape or Tab.
      if (!isTopDialog(token)) return

      if (event.key === 'Escape') {
        event.preventDefault()
        if (dismissibleRef.current) onOpenChangeRef.current(false)
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
      const index = dialogStack.indexOf(token)
      if (index !== -1) dialogStack.splice(index, 1)
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
    <div
      className={[
        'fixed inset-0 z-50 flex',
        variant === 'sheet' ? 'justify-end' : 'items-center justify-center p-4',
      ].join(' ')}
    >
      <div
        className="absolute inset-0 bg-overlay"
        aria-hidden="true"
        onClick={() => {
          if (dismissible) onOpenChange(false)
        }}
      />
      <div
        ref={panelRef}
        role={role}
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        className={[
          'relative w-full bg-surface-card text-ink-1 shadow-xl',
          variant === 'sheet'
            ? 'flex h-full max-w-[640px] flex-col overflow-y-auto border-l border-border-hairline p-6'
            : 'max-w-[440px] rounded-xl border border-border-hairline p-5',
          panelClassName,
        ]
          .filter(Boolean)
          .join(' ')}
      >
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 id={titleId} className="text-card-title">
              {title}
            </h2>
            {description ? (
              <p id={descriptionId} className="mt-1 text-body text-ink-2">
                {description}
              </p>
            ) : null}
          </div>
          {dismissible && closeLabel ? (
            <button
              type="button"
              aria-label={closeLabel}
              onClick={() => onOpenChange(false)}
              className="shrink-0 rounded-md p-1 leading-none text-ink-2 transition-colors hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              <span aria-hidden="true">×</span>
            </button>
          ) : null}
        </div>
        {children ? <div className="mt-4 text-body text-ink-2">{children}</div> : null}
        {footer ? <div className="mt-5 flex justify-end gap-2">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  )
}
