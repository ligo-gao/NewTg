import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type JSX,
  type KeyboardEvent as ReactKeyboardEvent,
  type RefObject,
} from 'react'
import { createPortal } from 'react-dom'

import { useI18n } from '@/shared/lib/i18n-context'

import type { Channel } from './types'

export interface ChannelRowActionsProps {
  channel: Channel
  busy: boolean
  onToggleStatus: () => void
  onTest: () => void
  onBatchTest: () => void
  onEdit: () => void
  onDelete: () => void
}

/** The viewport-space slice of the trigger rect the menu needs. */
interface AnchorRect {
  top: number
  bottom: number
  right: number
}

interface MenuPosition {
  top: number
  right: number
}

const SWITCH_BASE =
  'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50'

// The kebab stays focusable while the row is busy (`aria-disabled` rather than
// `disabled`): disabling it would blur the focused trigger and drop keyboard
// focus to <body> the moment an action chosen from its menu starts a write.
const KEBAB_CLASSES =
  'inline-flex size-9 shrink-0 items-center justify-center rounded-lg border border-transparent text-ink-2 transition-colors hover:border-border-hairline hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none aria-disabled:cursor-not-allowed aria-disabled:opacity-50'

const MENU_CLASSES =
  'fixed z-50 flex w-44 flex-col gap-0.5 rounded-lg border border-border-default bg-surface-card p-1 shadow-[0_12px_32px_var(--overlay)] focus-visible:outline-none'

const ITEM_BASE =
  'flex h-9 w-full items-center rounded-md px-3 text-left text-sm focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

const ITEM_CLASSES = `${ITEM_BASE} text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:bg-surface-hover focus-visible:text-ink-1`

const ITEM_DANGER_CLASSES = `${ITEM_BASE} text-danger hover:bg-danger-bg focus-visible:bg-danger-bg`

/** Clamp the menu inside the viewport on the narrow side. */
function menuPosition(anchor: AnchorRect): MenuPosition {
  return {
    top: anchor.bottom + 4,
    right: Math.max(8, window.innerWidth - anchor.right),
  }
}

interface ChannelActionMenuProps {
  anchor: AnchorRect
  menuRef: RefObject<HTMLDivElement | null>
  busy: boolean
  onSelect: (action: () => void) => void
  onClose: () => void
  onTest: () => void
  onBatchTest: () => void
  onEdit: () => void
  onDelete: () => void
}

/**
 * Portalled action menu.
 *
 * It lives in `document.body` because the table wrapper is `overflow-x-auto`
 * (and therefore `overflow-y: auto` too), which clips absolutely-positioned
 * descendants even when they extend below the wrapper. The frame is fixed and
 * anchored to the trigger's viewport rect; the flip-above pass runs once after
 * the first layout so a row near the bottom of the window still opens upward.
 */
function ChannelActionMenu({
  anchor,
  menuRef,
  busy,
  onSelect,
  onClose,
  onTest,
  onBatchTest,
  onEdit,
  onDelete,
}: ChannelActionMenuProps): JSX.Element {
  const { t } = useI18n()
  const [position, setPosition] = useState<MenuPosition>(() => menuPosition(anchor))

  useLayoutEffect(() => {
    const menu = menuRef.current
    if (menu === null) return
    const height = menu.getBoundingClientRect().height
    const spaceBelow = window.innerHeight - anchor.bottom
    if (height > spaceBelow - 8) {
      setPosition((current) => ({
        ...current,
        top: Math.max(8, anchor.top - height - 4),
      }))
    }
  }, [anchor, menuRef])

  // WAI-ARIA menu pattern: moving focus into the menu on open is mandatory for
  // screen readers, and gives arrow-key handling a starting point.
  useEffect(() => {
    const menu = menuRef.current
    if (menu === null) return
    const first = menu.querySelector<HTMLElement>('[role="menuitem"]:not(:disabled)')
    ;(first ?? menu).focus()
  }, [menuRef])

  const handleKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(
      menuRef.current?.querySelectorAll<HTMLButtonElement>(
        '[role="menuitem"]:not(:disabled)',
      ) ?? [],
    )
    if (items.length === 0) {
      if (event.key === 'Tab') onClose()
      return
    }
    const index = items.indexOf(document.activeElement as HTMLButtonElement)

    switch (event.key) {
      case 'ArrowDown': {
        event.preventDefault()
        items[index < 0 ? 0 : (index + 1) % items.length]?.focus()
        break
      }
      case 'ArrowUp': {
        event.preventDefault()
        items[index < 0 ? items.length - 1 : (index - 1 + items.length) % items.length]?.focus()
        break
      }
      case 'Home': {
        event.preventDefault()
        items[0]?.focus()
        break
      }
      case 'End': {
        event.preventDefault()
        items[items.length - 1]?.focus()
        break
      }
      case 'Tab': {
        // Close synchronously and hand focus back to the trigger; the browser's
        // default Tab then continues from the trigger into the next control.
        onClose()
        break
      }
      default:
        break
    }
  }

  const item = (
    label: string,
    action: () => void,
    danger = false,
  ): JSX.Element => (
    <button
      type="button"
      role="menuitem"
      disabled={busy}
      className={danger ? ITEM_DANGER_CLASSES : ITEM_CLASSES}
      onClick={() => {
        onSelect(action)
      }}
    >
      {label}
    </button>
  )

  return (
    <div
      ref={menuRef}
      role="menu"
      tabIndex={-1}
      aria-label={t('common.channels.actions.more')}
      className={MENU_CLASSES}
      style={{ top: position.top, right: position.right }}
      onKeyDown={handleKeyDown}
    >
      {item(t('common.channels.actions.test'), onTest)}
      {item(t('common.channels.actions.batchTest'), onBatchTest)}
      {item(t('common.channels.actions.edit'), onEdit)}
      {item(t('common.channels.actions.delete'), onDelete, true)}
    </div>
  )
}

/**
 * Per-row status switch plus a kebab menu for the row actions.
 *
 * The switch is the same control that used to sit inline in the list row, so
 * its ARIA contract (role, label, busy spinner test id) is preserved exactly.
 */
export function ChannelRowActions({
  channel,
  busy,
  onToggleStatus,
  onTest,
  onBatchTest,
  onEdit,
  onDelete,
}: ChannelRowActionsProps): JSX.Element {
  const { t } = useI18n()
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const [anchor, setAnchor] = useState<AnchorRect | null>(null)
  const open = anchor !== null

  const close = useCallback((restoreFocus = true) => {
    setAnchor(null)
    if (restoreFocus) triggerRef.current?.focus()
  }, [])

  // An action chosen from the menu can flip the row busy while the menu is
  // still open. Close it and hand focus back to the (still focusable) kebab
  // instead of letting a browser blur drop focus to <body>.
  useEffect(() => {
    if (busy && open) close()
  }, [busy, open, close])

  useEffect(() => {
    if (!open) return

    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (!(target instanceof Node)) return
      if (menuRef.current?.contains(target)) return
      if (triggerRef.current?.contains(target)) return
      close()
    }

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      event.preventDefault()
      close()
    }

    // The table (or any ancestor) may scroll under a fixed menu, which would
    // leave it detached from its trigger. Close normally so a keyboard user
    // lands back on the trigger; only skip the restore if the row is gone.
    const handleViewportChange = () => {
      close(triggerRef.current?.isConnected ?? false)
    }

    document.addEventListener('pointerdown', handlePointerDown, true)
    document.addEventListener('keydown', handleKeyDown)
    window.addEventListener('scroll', handleViewportChange, true)
    window.addEventListener('resize', handleViewportChange)

    return () => {
      document.removeEventListener('pointerdown', handlePointerDown, true)
      document.removeEventListener('keydown', handleKeyDown)
      window.removeEventListener('scroll', handleViewportChange, true)
      window.removeEventListener('resize', handleViewportChange)
    }
  }, [open, close])

  const handleTriggerClick = () => {
    if (open) {
      close()
      return
    }
    const rect = triggerRef.current?.getBoundingClientRect()
    if (rect === undefined) return
    setAnchor({ top: rect.top, bottom: rect.bottom, right: rect.right })
  }

  const handleSelect = (action: () => void) => {
    // Restore focus before running the action so a dialog opened by the action
    // remembers the kebab as its return target.
    close()
    action()
  }

  return (
    <div className="flex items-center gap-1">
      <button
        type="button"
        role="switch"
        aria-checked={channel.status === 1}
        aria-busy={busy}
        aria-label={t(
          channel.status === 1
            ? 'common.channels.actions.disableChannel'
            : 'common.channels.actions.enableChannel',
          { name: channel.name },
        )}
        disabled={busy}
        onClick={onToggleStatus}
        className={[
          SWITCH_BASE,
          channel.status === 1 ? 'bg-success' : 'bg-neutral-line',
        ].join(' ')}
      >
        {busy ? (
          <span
            data-testid={`channel-row-busy-${channel.id}`}
            aria-hidden="true"
            className="absolute inset-0 flex items-center justify-center"
          >
            <span className="size-3 animate-spin rounded-full border-2 border-on-brand border-t-transparent" />
          </span>
        ) : (
          <span
            aria-hidden="true"
            className={[
              'inline-block size-4 rounded-full bg-surface-card transition-transform',
              channel.status === 1 ? 'translate-x-4' : 'translate-x-0.5',
            ].join(' ')}
          />
        )}
      </button>

      <button
        ref={triggerRef}
        type="button"
        aria-label={t('common.channels.actions.moreFor', { name: channel.name })}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-disabled={busy}
        onClick={() => {
          if (busy) return
          handleTriggerClick()
        }}
        className={KEBAB_CLASSES}
      >
        <svg
          viewBox="0 0 16 16"
          aria-hidden="true"
          focusable="false"
          className="size-4"
          fill="currentColor"
        >
          <circle cx="8" cy="3" r="1.4" />
          <circle cx="8" cy="8" r="1.4" />
          <circle cx="8" cy="13" r="1.4" />
        </svg>
      </button>

      {anchor !== null
        ? createPortal(
            <ChannelActionMenu
              anchor={anchor}
              menuRef={menuRef}
              busy={busy}
              onSelect={handleSelect}
              onClose={() => {
                close()
              }}
              onTest={onTest}
              onBatchTest={onBatchTest}
              onEdit={onEdit}
              onDelete={onDelete}
            />,
            document.body,
          )
        : null}
    </div>
  )
}
