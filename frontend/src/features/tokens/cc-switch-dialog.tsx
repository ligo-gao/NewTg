import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { MASKED_KEY } from './api'

const APPS = ['claude', 'codex', 'gemini'] as const
type CcApp = (typeof APPS)[number]

const DEFAULT_NAMES: Record<CcApp, string> = {
  claude: 'My Claude',
  codex: 'My Codex',
  gemini: 'My Gemini',
}

export interface CcSwitchDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Full key, only available immediately after create/reset. */
  plaintextKey?: string | undefined
}

/**
 * CC Switch deep-link import (modules-02 §2.13, F-02-15).
 *
 * The link embeds the full key, so it is assembled only inside the click
 * handler and never persisted. When the caller has no plaintext key (the
 * management API never returns one outside create/reset, and the re-auth
 * endpoint in the design is not part of the frozen contract), the dialog
 * explains why import is unavailable instead of fabricating a link from the
 * mask.
 */
export function CcSwitchDialog({
  open,
  onOpenChange,
  plaintextKey,
}: CcSwitchDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [app, setApp] = useState<CcApp>('claude')
  const [name, setName] = useState(DEFAULT_NAMES.claude)
  const [primary, setPrimary] = useState('')
  const [haiku, setHaiku] = useState('')
  const [sonnet, setSonnet] = useState('')
  const [opus, setOpus] = useState('')
  const [error, setError] = useState<string | null>(null)

  function changeApp(next: CcApp): void {
    setApp(next)
    setName(DEFAULT_NAMES[next])
    setPrimary('')
    setHaiku('')
    setSonnet('')
    setOpus('')
    setError(null)
  }

  function buildDeepLink(key: string): string {
    const origin = typeof location !== 'undefined' ? location.origin : ''
    const endpoint = app === 'codex' ? `${origin}/v1` : origin
    const apiKey = key.startsWith('sk-') ? key : `sk-${key}`
    const params = new URLSearchParams({
      resource: 'provider',
      app,
      name,
      endpoint,
      apiKey,
      homepage: origin,
      enabled: 'true',
      model: primary,
    })
    if (app === 'claude') {
      if (haiku !== '') params.set('haikuModel', haiku)
      if (sonnet !== '') params.set('sonnetModel', sonnet)
      if (opus !== '') params.set('opusModel', opus)
    }
    return `ccswitch://v1/import?${params.toString()}`
  }

  function submit(): void {
    if (primary.trim() === '') {
      setError(t('common.tokens.ccSwitch.primaryRequired'))
      return
    }
    if (plaintextKey === undefined) {
      setError(t('common.tokens.ccSwitch.keyUnavailable'))
      return
    }
    const link = buildDeepLink(plaintextKey)
    try {
      window.location.href = link
    } catch {
      push({ tone: 'error', message: t('common.tokens.toast.copyFailed') })
    }
    onOpenChange(false)
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('common.tokens.ccSwitch.title')}
      footer={
        <>
          <Button
            variant="secondary"
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('common.tokens.ccSwitch.cancel')}
          </Button>
          <Button onClick={submit} disabled={plaintextKey === undefined}>
            {t('common.tokens.ccSwitch.import')}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <fieldset className="flex flex-col gap-2">
          <legend className="text-[13px] font-medium text-ink-1">
            {t('common.tokens.ccSwitch.application')}
          </legend>
          <div className="flex gap-4">
            {APPS.map((value) => (
              <label key={value} className="flex items-center gap-1.5 text-sm">
                <input
                  type="radio"
                  name="cc-switch-app"
                  value={value}
                  checked={app === value}
                  onChange={() => {
                    changeApp(value)
                  }}
                />
                {value}
              </label>
            ))}
          </div>
        </fieldset>

        <TextField
          label={t('common.tokens.ccSwitch.name')}
          value={name}
          onChange={(event) => {
            setName(event.target.value)
          }}
        />
        <TextField
          label={t('common.tokens.ccSwitch.primary')}
          value={primary}
          error={error ?? undefined}
          onChange={(event) => {
            setPrimary(event.target.value)
          }}
        />
        {app === 'claude' ? (
          <>
            <TextField
              label={t('common.tokens.ccSwitch.haiku')}
              value={haiku}
              onChange={(event) => setHaiku(event.target.value)}
            />
            <TextField
              label={t('common.tokens.ccSwitch.sonnet')}
              value={sonnet}
              onChange={(event) => setSonnet(event.target.value)}
            />
            <TextField
              label={t('common.tokens.ccSwitch.opus')}
              value={opus}
              onChange={(event) => setOpus(event.target.value)}
            />
          </>
        ) : null}

        <div className="flex items-center gap-2">
          <span className="text-xs text-ink-2">{t('common.tokens.form.key')}</span>
          <code className="rounded bg-surface-subtle px-2 py-1 font-mono text-xs text-ink-2">
            {MASKED_KEY}
          </code>
        </div>
        <div
          role="note"
          className="rounded-md border-l-4 border-warning bg-warning-bg px-3 py-2 text-xs text-warning"
        >
          <p>{t('common.tokens.ccSwitch.warning')}</p>
          <p className="mt-1">{t('common.tokens.ccSwitch.notInstalled')}</p>
        </div>
        {plaintextKey === undefined ? (
          <p role="alert" className="text-xs text-danger">
            {t('common.tokens.ccSwitch.keyUnavailable')}
          </p>
        ) : null}
      </div>
    </Dialog>
  )
}
