import { useEffect, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { MASKED_KEY } from './api'

const APPS = ['claude', 'codex', 'gemini'] as const
type CcApp = (typeof APPS)[number]

const DEFAULT_NAME_KEYS: Record<CcApp, string> = {
  claude: 'common.tokens.ccSwitch.defaultClaude',
  codex: 'common.tokens.ccSwitch.defaultCodex',
  gemini: 'common.tokens.ccSwitch.defaultGemini',
}

export interface CcSwitchDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /**
   * Full key from the one-time reveal dialog. Existing tokens cannot supply
   * one: the frozen contract has no re-authentication key endpoint, so this
   * dialog is only reachable right after create/reset.
   */
  plaintextKey: string
}

/**
 * CC Switch deep-link import (modules-02 §2.13, F-02-15).
 *
 * The link embeds the full key, so it is assembled only inside the click
 * handler and never persisted. State is reset on every open so a previous
 * application/model mapping cannot leak into the next import.
 */
export function CcSwitchDialog({
  open,
  onOpenChange,
  plaintextKey,
}: CcSwitchDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [app, setApp] = useState<CcApp>('claude')
  const [name, setName] = useState('')
  const [primary, setPrimary] = useState('')
  const [haiku, setHaiku] = useState('')
  const [sonnet, setSonnet] = useState('')
  const [opus, setOpus] = useState('')
  const [error, setError] = useState<string | null>(null)

  function defaultName(next: CcApp): string {
    return t(DEFAULT_NAME_KEYS[next])
  }

  useEffect(() => {
    if (!open) return
    setApp('claude')
    setName(t(DEFAULT_NAME_KEYS.claude))
    setPrimary('')
    setHaiku('')
    setSonnet('')
    setOpus('')
    setError(null)
  }, [open, t])

  function changeApp(next: CcApp): void {
    setApp(next)
    setName(defaultName(next))
    setPrimary('')
    setHaiku('')
    setSonnet('')
    setOpus('')
    setError(null)
  }

  function buildDeepLink(key: string): string {
    const origin = typeof location !== 'undefined' ? location.origin : ''
    const endpoint = app === 'codex' ? `${origin}/v1` : origin
    // 新令牌明文以 `sk-` 开头；历史令牌可能是 `nkt-`。两者都按完整明文
    // 哈希校验，深链接直接使用原始 key，不要再拼前缀。
    const apiKey = key
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
      // Claude Code 还会用小模型处理后台任务；留空会回落到 cc-switch 的
      // 默认 Claude 模型，而本网关没有该渠道（503）。未填时统一回落到主模型。
      params.set('haikuModel', haiku !== '' ? haiku : primary)
      params.set('sonnetModel', sonnet !== '' ? sonnet : primary)
      params.set('opusModel', opus !== '' ? opus : primary)
    }
    return `ccswitch://v1/import?${params.toString()}`
  }

  function submit(): void {
    if (primary.trim() === '') {
      setError(t('common.tokens.ccSwitch.primaryRequired'))
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
          <Button onClick={submit}>{t('common.tokens.ccSwitch.import')}</Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <fieldset className="flex flex-col gap-2">
          <legend className="text-meta font-medium text-ink-1">
            {t('common.tokens.ccSwitch.application')}
          </legend>
          <div className="flex gap-4">
            {APPS.map((value) => (
              <label key={value} className="flex items-center gap-1.5 text-body">
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
          <span className="text-meta text-ink-2">{t('common.tokens.form.key')}</span>
          <code className="rounded bg-surface-subtle px-2 py-1 font-mono text-meta text-ink-2">
            {MASKED_KEY}
          </code>
        </div>
        <div
          role="note"
          className="rounded-md border-l-4 border-warning bg-warning-bg px-3 py-2 text-meta text-warning"
        >
          <p>{t('common.tokens.ccSwitch.warning')}</p>
          <p className="mt-1">{t('common.tokens.ccSwitch.notInstalled')}</p>
        </div>
      </div>
    </Dialog>
  )
}
