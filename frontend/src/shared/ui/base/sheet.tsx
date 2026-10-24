import { useEffect, useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

export interface SheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
  /**
   * 面板最大宽度类。默认是 480px 的窄抽屉；工作台类表单（例如定价时段
   * 编辑器）需要更宽的画布，调用方可传 `max-w-[1100px]` 而不复制面板。
   */
  widthClassName?: string
}

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableInside(panel: HTMLElement): HTMLElement[] {
  return Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (element) =>
      // 与 Dialog 对齐（UI-5 评审）：`[tabindex]` 选择器已排除 -1，但元素
      // 仍可能通过负值属性以外的途径不可聚焦；aria-hidden 的子树也不能
      // 成为陷阱的落点，否则 Tab 会把焦点送进视觉上不存在的控件。
      element.tabIndex !== -1 &&
      element.getAttribute('aria-hidden') !== 'true' &&
      // jsdom 没有布局；display:none 的元素不产生盒子，getClientRects()
      // 为空，据此把隐藏候选排除出首/末位置。
      element.getClientRects().length > 0,
  )
}

/**
 * Minimal right-hand Sheet primitive (web-00 §2.1, modules-00 §4.8).
 *
 * It mirrors the M0 Dialog's modal lifecycle (portal, inert background, focus
 * capture/restore, Escape and Tab containment) but renders the panel on the
 * right edge. Kept separate from Dialog so the M0 dialog behaviour and its
 * regression tests stay untouched.
 */
export function Sheet({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  widthClassName = 'max-w-[480px]',
}: SheetProps) {
  const panelRef = useRef<HTMLDivElement>(null)
  const titleId = useId()
  const descriptionId = useId()
  const onOpenChangeRef = useRef(onOpenChange)

  useEffect(() => {
    onOpenChangeRef.current = onOpenChange
  }, [onOpenChange])

  useEffect(() => {
    if (!open) return
    const panel = panelRef.current
    if (panel === null) return

    const previouslyFocused =
      document.activeElement instanceof HTMLElement ? document.activeElement : null
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
        event.preventDefault()
        panel.focus()
        return
      }
      const active = document.activeElement
      const outside = active === null || !panel.contains(active)
      if (event.shiftKey) {
        if (outside || active === first || active === panel) {
          event.preventDefault()
          last.focus()
        }
        return
      }
      if (outside || active === last || active === panel) {
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
      if (previouslyFocused?.isConnected) previouslyFocused.focus()
    }
  }, [open])

  if (!open) return null

  return createPortal(
    <div className="fixed inset-0 z-50 flex justify-end">
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
        className={`relative flex h-full w-full ${widthClassName} flex-col overflow-y-auto border-l border-border-hairline bg-surface-card p-5 text-ink-1 shadow-xl`}
      >
        <h2 id={titleId} className="text-card-title">
          {title}
        </h2>
        {description ? (
          <p id={descriptionId} className="mt-1 text-body text-ink-2">
            {description}
          </p>
        ) : null}
        {children ? <div className="mt-4 flex-1 text-body text-ink-2">{children}</div> : null}
        {footer ? <div className="mt-5 flex justify-end gap-2">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  )
}
