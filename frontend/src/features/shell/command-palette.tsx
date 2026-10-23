import { useQuery } from '@tanstack/react-query'
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
} from 'react'

import { meQueryOptions } from '@/lib/session'
import { useDocsVisible } from '@/features/public/nav-visibility'
import { writeClipboard } from '@/features/tokens/clipboard'
import { usePlaygroundEnabled } from '@/features/playground/visibility'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'
import { Dialog } from '@/shared/ui/base/dialog'

import {
  COMMANDS,
  COMMAND_GROUP_LABEL_KEYS,
  isCommandAllowed,
  matchCommand,
  type CommandDefinition,
  type CommandGroupId,
  type CommandRoutePath,
} from './commands'

const GROUP_ORDER: readonly CommandGroupId[] = ['pages', 'actions']

/** §15.1.2 caps: five per group, twenty overall. */
const GROUP_LIMIT = 5
const TOTAL_LIMIT = 20

interface PaletteItem {
  command: CommandDefinition
  label: string
  score: number
}

interface PaletteGroup {
  id: CommandGroupId
  items: PaletteItem[]
}

export interface CommandPaletteProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Client-side navigation supplied by the shell; no request is made. */
  onNavigate: (to: CommandRoutePath) => void
}

/**
 * Single command palette instance for the console shell (web-00 §15.1.1).
 *
 * RBAC filtering happens in `useMemo` *before* the result list is built, so an
 * unauthorized entry never enters the DOM, the result count or the live
 * announcement (§15.1.4). While `['me']` is pending or failed, `me` is
 * `undefined` and `isCommandAllowed` fails closed for every privileged entry.
 */
export function CommandPalette({
  open,
  onOpenChange,
  onNavigate,
}: CommandPaletteProps) {
  const { t } = useI18n()
  const meQuery = useQuery(meQueryOptions())
  const me = meQuery.data
  // Reads the root guard's `['public','status']` snapshot without a request.
  // The route guard still protects direct `/playground` links; hiding here
  // avoids offering a command that would bounce straight back to `/dashboard`.
  const playgroundEnabled = usePlaygroundEnabled()
  // Same rule for Docs: no configured target, no command (spec §4). The
  // `/docs` route guard remains authoritative for direct links.
  const docsVisible = useDocsVisible()

  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const optionRefs = useRef(new Map<string, HTMLDivElement>())

  const groups = useMemo<PaletteGroup[]>(() => {
    const byGroup = new Map<CommandGroupId, PaletteItem[]>(
      GROUP_ORDER.map((id) => [id, []]),
    )

    for (const command of COMMANDS) {
      if (command.requiresPlayground === true && !playgroundEnabled) continue
      if (command.requiresDocs === true && !docsVisible) continue
      if (!isCommandAllowed(command, me)) continue
      const label = t(command.labelKey)
      const score = matchCommand(command, query, label)
      if (score === null) continue
      byGroup.get(command.group)?.push({ command, label, score })
    }

    let remaining = TOTAL_LIMIT
    return GROUP_ORDER.map((id) => {
      const items = byGroup.get(id) ?? []
      // Stable ranking: score first, registry order on ties (§15.1.2).
      const ranked = items
        .map((item, index) => ({ item, index }))
        .sort((a, b) => b.item.score - a.item.score || a.index - b.index)
        .map((entry) => entry.item)
      const limited = ranked.slice(0, Math.min(GROUP_LIMIT, remaining))
      remaining -= limited.length
      return { id, items: limited }
    })
  }, [me, query, t, playgroundEnabled, docsVisible])

  const flatItems = useMemo(
    () => groups.flatMap((group) => group.items),
    [groups],
  )

  const safeIndex = flatItems.length === 0
    ? -1
    : Math.min(activeIndex, flatItems.length - 1)
  const activeItem = safeIndex >= 0 ? flatItems[safeIndex] : undefined

  // Opening always starts from a clean query; a stale query would make the
  // first Cmd+K after a previous run look like a remembered search.
  useEffect(() => {
    if (!open) return
    setQuery('')
    setActiveIndex(0)
    inputRef.current?.focus()
  }, [open])

  useEffect(() => {
    setActiveIndex(0)
  }, [query])

  useEffect(() => {
    if (activeItem === undefined) return
    optionRefs.current
      .get(activeItem.command.id)
      ?.scrollIntoView?.({ block: 'nearest' })
  }, [activeItem])

  async function execute(item: PaletteItem): Promise<void> {
    // Close first so the dialog's focus restore runs before the route change
    // (§15.1.3); the navigation callback is a pure client-side push.
    onOpenChange(false)
    if (item.command.action === 'copy-page-url') {
      // FE-02 clipboard semantics: the success toast is only shown after the
      // write resolves, and a denied/absent clipboard reports an error instead
      // of claiming a copy that never happened.
      const copied = await writeClipboard(window.location.href)
      useToastStore.getState().push({
        tone: copied ? 'success' : 'error',
        message: copied
          ? t('common.command.actions.copied')
          : t('common.command.actions.copy_failed'),
      })
      return
    }
    if (item.command.to !== undefined) onNavigate(item.command.to)
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    // IME composition must not execute or navigate (web-00 §15.1.3).
    if (event.nativeEvent.isComposing) return

    switch (event.key) {
      case 'Escape':
        // First Escape clears the query; the dialog's own Escape handler then
        // closes on the next one. Stopping propagation keeps both from firing
        // on the same keypress.
        if (query !== '') {
          event.preventDefault()
          event.stopPropagation()
          setQuery('')
          setActiveIndex(0)
        }
        return
      case 'ArrowDown':
        if (flatItems.length === 0) return
        event.preventDefault()
        setActiveIndex((safeIndex + 1) % flatItems.length)
        return
      case 'ArrowUp':
        if (flatItems.length === 0) return
        event.preventDefault()
        setActiveIndex(
          (safeIndex - 1 + flatItems.length) % flatItems.length,
        )
        return
      case 'Home':
        if (flatItems.length === 0) return
        event.preventDefault()
        setActiveIndex(0)
        return
      case 'End':
        if (flatItems.length === 0) return
        event.preventDefault()
        setActiveIndex(flatItems.length - 1)
        return
      case 'Enter':
        if (activeItem === undefined) return
        event.preventDefault()
        void execute(activeItem)
        return
      default:
        return
    }
  }

  const resultCountLabel = t('common.command.result_count', {
    count: flatItems.length,
  })

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('common.command.title')}
      panelClassName="max-w-[560px]"
    >
      <div>
        <input
          ref={inputRef}
          type="text"
          role="combobox"
          aria-expanded="true"
          aria-controls="command-listbox"
          aria-autocomplete="list"
          aria-label={t('common.command.placeholder')}
          aria-activedescendant={
            activeItem === undefined
              ? undefined
              : `command-option-${activeItem.command.id}`
          }
          placeholder={t('common.command.placeholder')}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={handleKeyDown}
          className="w-full rounded-lg border border-border-default bg-surface-page px-3 py-2 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        />

        <p aria-live="polite" className="sr-only">
          {resultCountLabel}
        </p>

        <div
          id="command-listbox"
          role="listbox"
          aria-label={t('common.command.list_label')}
          className="mt-3 max-h-[320px] overflow-y-auto"
        >
          {flatItems.length === 0 ? (
            <p role="status" className="px-1 py-6 text-sm text-ink-2">
              {t('common.command.no_results')}
            </p>
          ) : (
            groups.map((group) =>
              group.items.length === 0 ? null : (
                <div
                  key={group.id}
                  role="group"
                  aria-labelledby={`command-group-${group.id}`}
                >
                  <p
                    id={`command-group-${group.id}`}
                    className="px-1 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wide text-ink-3"
                  >
                    {t(COMMAND_GROUP_LABEL_KEYS[group.id])}
                  </p>
                  {group.items.map((item) => {
                    const isActive = activeItem?.command.id === item.command.id
                    return (
                      <div
                        key={item.command.id}
                        id={`command-option-${item.command.id}`}
                        ref={(element) => {
                          if (element === null) {
                            optionRefs.current.delete(item.command.id)
                          } else {
                            optionRefs.current.set(item.command.id, element)
                          }
                        }}
                        role="option"
                        aria-selected={isActive}
                        data-analytics-id={item.command.analyticsId}
                        onMouseEnter={() => {
                          const index = flatItems.findIndex(
                            (candidate) =>
                              candidate.command.id === item.command.id,
                          )
                          if (index >= 0) setActiveIndex(index)
                        }}
                        onClick={() => {
                          void execute(item)
                        }}
                        className={[
                          'cursor-pointer rounded-lg px-2 py-1.5 text-sm',
                          isActive
                            ? 'bg-surface-hover text-ink-1'
                            : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                        ].join(' ')}
                      >
                        {item.label}
                      </div>
                    )
                  })}
                </div>
              ),
            )
          )}
        </div>
      </div>
    </Dialog>
  )
}

/**
 * One global Cmd+K / Ctrl+K listener for the console shell (web-00 §15.1.1).
 *
 * Mounted once by `ConsoleLayout`; the effect cleanup removes it on unmount.
 * The handler ignores IME composition, modified combinations we do not own, and
 * any open dialog/sheet so the palette never stacks on top of another trap.
 * Cmd+K intentionally works while typing in a field — it is a global shortcut —
 * while Escape is left entirely to the dialog layer.
 */
export function useCommandPaletteShortcut(onOpen: () => void): void {
  const onOpenRef = useRef(onOpen)

  useEffect(() => {
    onOpenRef.current = onOpen
  }, [onOpen])

  useEffect(() => {
    function handleKeyDown(event: globalThis.KeyboardEvent): void {
      if (event.defaultPrevented || event.isComposing) return
      if (!(event.metaKey || event.ctrlKey)) return
      if (event.altKey || event.shiftKey) return
      if (event.key.toLowerCase() !== 'k') return
      if (document.querySelector('[role="dialog"]') !== null) return

      event.preventDefault()
      onOpenRef.current()
    }

    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [])
}
