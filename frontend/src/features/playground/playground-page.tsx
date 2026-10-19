import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { meQueryOptions } from '@/lib/session'
import { publicStatusQueryOptions } from '@/lib/public-status'
import { selfSettingsQueryOptions } from '@/lib/self-settings'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { useToastStore } from '@/shared/ui/toast'

import {
  PlaygroundError,
  fetchTurnCredits,
  isAbortError,
  streamChatCompletion,
  type PlaygroundChatMessage,
} from './api'
import { MessageContent } from './message-content'
import {
  clearPlaygroundMessages,
  loadPlaygroundMessages,
  playgroundSessionKey,
  savePlaygroundMessages,
  type PlaygroundMessage,
} from './session-storage'
import type { UsableGroup } from '@/features/model-config/api'

/**
 * 与充值页共用的钱包缓存键（F-03-01）。键形状必须一致，完成一轮对话后
 * 失效才会让钱包页/本页余额同时刷新。
 */
const WALLET_SUMMARY_KEY = ['wallet', 'summary'] as const

type PlaygroundFailure =
  | { kind: 'quota' }
  | { kind: 'forbidden' }
  | { kind: 'auth' }
  | { kind: 'server'; requestId?: string }
  | { kind: 'other'; requestId?: string; message?: string }

function newId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `msg-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

/**
 * Playground 在线调试（modules-02 §2.16，F-02-19）。
 *
 * 计费边界是硬约束，页面只做呈现、不重算：身份 = 会话（无 Authorization、
 * `usage_logs.token_id` 为 NULL），跳过令牌额度与令牌白名单，但钱包/订阅
 * 照常扣费、违规费不豁免。请求成功后按 `X-Request-ID` 回读 `usage_logs`
 * 拿本轮 credits，绝不用本地 tokenizer 估算。
 */
export function PlaygroundPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const statusQuery = useQuery(publicStatusQueryOptions())
  const settingsQuery = useQuery(selfSettingsQueryOptions())
  const meQuery = useQuery(meQueryOptions())
  const walletQuery = useQuery({
    queryKey: WALLET_SUMMARY_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
  })

  const groups: UsableGroup[] = statusQuery.data?.usable_groups ?? []
  const availableModels = groups.flatMap((group) => group.models)
  const availableSet = new Set(availableModels)
  const defaultModel = settingsQuery.data?.default_model ?? ''
  const username = meQuery.data?.username ?? ''

  const [model, setModel] = useState('')
  const [messages, setMessages] = useState<PlaygroundMessage[]>([])
  const [draft, setDraft] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [failure, setFailure] = useState<PlaygroundFailure | null>(null)
  const [announcement, setAnnouncement] = useState('')
  const controllerRef = useRef<AbortController | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  /** 用户是否手动选过模型：手动选择后，服务端默认模型到达也不能覆盖它。 */
  const pickedModelRef = useRef(false)

  // 初始模型：default_model（仍可用）→ 首个可用模型（§2.16 主流程 1）。
  // 必须等 GET /api/self 落定再决定，否则会先选中首个模型、再被默认模型
  // 覆盖，页面出现一次可见的闪跳。依赖 availableKey 而非数组本身，避免
  // 每次渲染新建数组导致循环。
  const availableKey = availableModels.join('\u0000')
  const defaultsReady = !settingsQuery.isPending
  useEffect(() => {
    if (!defaultsReady) return
    if (pickedModelRef.current && model !== '' && availableSet.has(model)) {
      return
    }
    const fallback =
      defaultModel !== '' && availableSet.has(defaultModel)
        ? defaultModel
        : availableModels[0] ?? ''
    if (fallback !== model) setModel(fallback)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- availableSet 由 availableKey 派生
  }, [availableKey, defaultModel, defaultsReady, model])

  const storageKey =
    username !== '' && model !== ''
      ? playgroundSessionKey(username, model)
      : null

  // 每个「用户 + 模型」组合各自一条会话；切换模型时载入对应历史。
  useEffect(() => {
    if (storageKey === null) {
      setMessages([])
      return
    }
    setMessages(loadPlaygroundMessages(storageKey))
  }, [storageKey])

  function commit(next: PlaygroundMessage[]): void {
    setMessages(next)
    if (storageKey !== null) savePlaygroundMessages(storageKey, next)
  }

  function failFromError(error: PlaygroundError): void {
    if (error.status === 402) {
      setFailure({ kind: 'quota' })
      return
    }
    if (error.status === 403) {
      setFailure({ kind: 'forbidden' })
      return
    }
    if (error.status === 401) {
      setFailure({ kind: 'auth' })
      return
    }
    if (error.status >= 500) {
      setFailure({ kind: 'server', requestId: error.requestId })
      return
    }
    setFailure({
      kind: 'other',
      requestId: error.requestId,
      message: error.message,
    })
  }

  async function handleSend(): Promise<void> {
    const text = draft.trim()
    if (text === '' || streaming || model === '' || storageKey === null) return

    const prior = messages
    const userMessage: PlaygroundMessage = {
      id: newId(),
      role: 'user',
      content: text,
    }
    const assistantId = newId()
    setFailure(null)
    setAnnouncement('')
    setDraft('')
    setMessages([...prior, userMessage, {
      id: assistantId,
      role: 'assistant',
      content: '',
      model,
    }])
    setStreaming(true)

    const controller = new AbortController()
    controllerRef.current = controller
    let accumulated = ''

    const requestMessages: PlaygroundChatMessage[] = [
      ...prior.map((message) => ({
        role: message.role,
        content: message.content,
      })),
      { role: 'user', content: text },
    ]

    try {
      const result = await streamChatCompletion({
        model,
        messages: requestMessages,
        signal: controller.signal,
        onToken: (delta) => {
          accumulated += delta
          setMessages((previous) =>
            previous.map((message) =>
              message.id === assistantId
                ? { ...message, content: message.content + delta }
                : message,
            ),
          )
        },
      })

      const credits =
        result.requestId !== undefined
          ? await fetchTurnCredits(result.requestId)
          : null
      const assistant: PlaygroundMessage = {
        id: assistantId,
        role: 'assistant',
        content: result.content,
        model,
        stopped: false,
      }
      if (result.usage !== undefined) {
        assistant.usage = {
          promptTokens: result.usage.prompt_tokens,
          completionTokens: result.usage.completion_tokens,
          credits,
        }
      }
      commit([...prior, userMessage, assistant])
      setAnnouncement(t('account.playground.reply_ready'))
      // 余额在每次完成后刷新（不本地递增，避免与服务端结算分叉）。
      void queryClient.invalidateQueries({ queryKey: WALLET_SUMMARY_KEY })
    } catch (error) {
      if (isAbortError(error)) {
        // 停止：保留已生成内容并标记；服务端按实际 usage 结算，本地不估算。
        const assistant: PlaygroundMessage = {
          id: assistantId,
          role: 'assistant',
          content: accumulated,
          model,
          stopped: true,
        }
        const next =
          accumulated === ''
            ? [...prior, userMessage]
            : [...prior, userMessage, assistant]
        commit(next)
        setAnnouncement(t('account.playground.stopped'))
      } else if (error instanceof PlaygroundError) {
        if (error.status === 429) {
          // 限流不代表消息已受理：回滚本轮两条消息并保留输入。
          commit(prior)
          setDraft(text)
          push({ tone: 'error', message: t('account.playground.rate_limited') })
        } else {
          // 402/403/5xx：用户消息保留在会话里，错误卡解释原因。
          commit([...prior, userMessage])
          failFromError(error)
          setAnnouncement(t('account.playground.error_title'))
        }
      } else {
        // 传输失败（离线）：回滚并保留草稿。
        commit(prior)
        setDraft(text)
        push({ tone: 'error', message: t('account.playground.offline') })
      }
    } finally {
      setStreaming(false)
      controllerRef.current = null
    }
  }

  function handleStop(): void {
    controllerRef.current?.abort()
  }

  function handleClear(): void {
    if (streaming || storageKey === null) return
    clearPlaygroundMessages(storageKey)
    setMessages([])
    setFailure(null)
    setAnnouncement(t('account.playground.session_cleared'))
  }

  const balance = walletQuery.data?.balance ?? null
  const noModels = availableModels.length === 0
  const sendDisabled = streaming || draft.trim() === '' || noModels || model === ''

  return (
    <div className="mx-auto flex max-w-4xl flex-col gap-4">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('account.playground.title')}
        </h1>
        <div className="flex flex-wrap items-center gap-2">
          <label className="sr-only" htmlFor="playground-model">
            {t('account.playground.select_model')}
          </label>
          <select
            id="playground-model"
            value={model}
            disabled={streaming || noModels}
            onChange={(event) => {
              pickedModelRef.current = true
              setModel(event.target.value)
            }}
            className="h-9 min-w-[220px] rounded-lg border border-border-default bg-surface-page px-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-60"
          >
            {noModels ? (
              <option value="">{t('account.playground.no_models')}</option>
            ) : null}
            {groups.map((group) => (
              <optgroup key={group.name} label={group.name}>
                {group.models.map((name) => (
                  <option key={`${group.name}:${name}`} value={name}>
                    {name}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>

          <a
            href="/wallet"
            className="inline-flex h-9 items-center gap-1 rounded-lg border border-border-default bg-surface-card px-3 text-[13px] text-ink-2 hover:bg-surface-hover hover:text-ink-1"
            aria-label={t('account.playground.balance_link')}
          >
            <span className="text-ink-3">{t('account.playground.balance')}:</span>
            <span className="font-mono text-ink-1" data-testid="playground-balance">
              {balance === null ? '—' : formatCredits(balance, locale)}
            </span>
          </a>

          <Button
            variant="secondary"
            onClick={handleClear}
            disabled={streaming || messages.length === 0}
          >
            {t('account.playground.clear')}
          </Button>
          <a
            href="/model-config?focus=default-model"
            className="text-[13px] text-brand underline-offset-2 hover:underline"
          >
            {t('account.playground.default_model_link')}
          </a>
        </div>
      </header>

      <div
        role="note"
        className="rounded-lg border border-info-line bg-info-bg px-3 py-2 text-xs text-ink-2"
      >
        <p>{t('account.playground.billing_notice')}</p>
        <p>{t('account.playground.billing_quota')}</p>
        <p>{t('account.playground.billing_violation')}</p>
      </div>

      <p role="status" aria-live="polite" className="sr-only">
        {announcement}
      </p>

      <section
        aria-label={t('account.playground.messages_label')}
        role="log"
        aria-live={streaming ? 'off' : 'polite'}
        aria-relevant="additions"
        className="min-h-[320px] space-y-3 rounded-xl border border-border-hairline bg-surface-card p-4"
      >
        {messages.length === 0 ? (
          <EmptyState
            onPick={(prompt) => {
              setDraft(prompt)
              textareaRef.current?.focus()
            }}
          />
        ) : (
          messages.map((message) => (
            <article
              key={message.id}
              data-testid={`message-${message.role}`}
              className={[
                'rounded-lg px-3 py-2',
                message.role === 'user'
                  ? 'ml-auto max-w-[85%] bg-surface-subtle text-ink-1'
                  : 'mr-auto max-w-full bg-surface-card text-ink-1',
              ].join(' ')}
            >
              <p className="mb-1 text-[11px] font-medium text-ink-3">
                {message.role === 'user'
                  ? t('account.playground.user_label')
                  : t('account.playground.assistant_label')}
                {message.role === 'assistant' && message.model !== undefined
                  ? ` · ${message.model}`
                  : ''}
              </p>
              {message.role === 'assistant' ? (
                <MessageContent text={message.content} />
              ) : (
                <p className="whitespace-pre-wrap break-words text-sm text-ink-1">
                  {message.content}
                </p>
              )}
              {message.stopped === true ? (
                <p className="mt-1 text-xs text-warning">
                  {t('account.playground.stopped')}
                </p>
              ) : null}
              {message.usage !== undefined ? (
                <p className="mt-1 font-mono text-[11px] text-ink-3">
                  {t('account.playground.usage_prompt', {
                    count: message.usage.promptTokens,
                  })}
                  {' · '}
                  {t('account.playground.usage_completion', {
                    count: message.usage.completionTokens,
                  })}
                  {' · '}
                  {message.usage.credits === null
                    ? t('account.playground.usage_pending')
                    : t('account.playground.turn_credits', {
                        count: message.usage.credits,
                      })}
                  {' · '}
                  {t('account.playground.usage_token_null')}
                </p>
              ) : null}
            </article>
          ))
        )}
        {streaming ? (
          <p className="text-xs text-ink-3" data-testid="playground-streaming">
            {t('account.playground.streaming')}
            <span
              aria-hidden="true"
              className="ml-1 inline-block animate-pulse motion-reduce:animate-none"
            >
              ▍
            </span>
          </p>
        ) : null}
      </section>

      {failure !== null ? (
        <FailureCard failure={failure} />
      ) : null}

      <section className="rounded-xl border border-border-hairline bg-surface-card p-3">
        <label
          htmlFor="playground-input"
          className="text-[13px] font-medium text-ink-1"
        >
          {t('account.playground.input_label')}
        </label>
        <textarea
          id="playground-input"
          ref={textareaRef}
          value={draft}
          rows={3}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              void handleSend()
            }
          }}
          placeholder={
            noModels
              ? t('account.playground.no_models')
              : t('account.playground.input_placeholder')
          }
          aria-describedby="playground-input-hint"
          className="mt-1 min-h-[72px] w-full resize-y rounded-lg border border-border-default bg-surface-page px-3 py-2 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        />
        <div className="mt-2 flex items-center justify-between gap-2">
          <p id="playground-input-hint" className="text-xs text-ink-3">
            {t('account.playground.input_hint')}
          </p>
          <div className="flex gap-2">
            {streaming ? (
              <Button variant="secondary" onClick={handleStop}>
                {t('account.playground.stop')}
              </Button>
            ) : null}
            <Button onClick={() => void handleSend()} disabled={sendDisabled}>
              {t('account.playground.send')}
            </Button>
          </div>
        </div>
      </section>
    </div>
  )
}

function EmptyState({ onPick }: { onPick: (prompt: string) => void }) {
  const { t } = useI18n()
  const examples = [
    t('account.playground.example_1'),
    t('account.playground.example_2'),
    t('account.playground.example_3'),
  ]
  return (
    <div className="mx-auto max-w-xl py-8 text-center" data-testid="playground-welcome">
      <h2 className="text-base font-semibold text-ink-1">
        {t('account.playground.welcome_title')}
      </h2>
      <p className="mt-1 text-[13px] text-ink-2">
        {t('account.playground.welcome_description')}
      </p>
      <p className="mt-4 text-xs font-medium text-ink-3">
        {t('account.playground.example_label')}
      </p>
      <div className="mt-2 flex flex-col gap-2">
        {examples.map((example) => (
          <button
            key={example}
            type="button"
            onClick={() => onPick(example)}
            className="rounded-lg border border-border-default bg-surface-page px-3 py-2 text-left text-[13px] text-ink-2 hover:bg-surface-hover hover:text-ink-1"
          >
            {example}
          </button>
        ))}
      </div>
    </div>
  )
}

function FailureCard({ failure }: { failure: PlaygroundFailure }) {
  const { t } = useI18n()

  if (failure.kind === 'quota') {
    return (
      <div role="alert" className="rounded-lg border border-warning-line bg-warning-bg p-3 text-[13px]">
        <p className="font-medium text-ink-1">
          {t('account.playground.insufficient')}
        </p>
        <a
          href="/wallet"
          className="mt-2 inline-block text-brand underline-offset-2 hover:underline"
        >
          {t('account.playground.topup_action')}
        </a>
      </div>
    )
  }
  if (failure.kind === 'forbidden') {
    return (
      <div role="alert" className="rounded-lg border border-warning-line bg-warning-bg p-3 text-[13px]">
        <p className="font-medium text-ink-1">
          {t('account.playground.unavailable')}
        </p>
        <p className="mt-1 text-xs text-ink-2">
          {t('account.playground.no_bill')}
        </p>
      </div>
    )
  }
  if (failure.kind === 'auth') {
    return (
      <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-[13px]">
        <p className="font-medium text-ink-1">
          {t('common.error401.title')}
        </p>
        <a
          href="/sign-in"
          className="mt-2 inline-block text-brand underline-offset-2 hover:underline"
        >
          {t('common.error401.sign_in')}
        </a>
      </div>
    )
  }
  return (
    <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-[13px]">
      <p className="font-medium text-ink-1">
        {t('account.playground.error_title')}
      </p>
      {failure.kind === 'other' && failure.message !== undefined ? (
        <p className="mt-1 text-xs text-ink-2">{failure.message}</p>
      ) : null}
      {failure.requestId !== undefined ? (
        <p className="mt-1 font-mono text-[11px] text-ink-3">
          {t('account.playground.request_id', { id: failure.requestId })}
        </p>
      ) : null}
      <a
        href="/wallet"
        className="mt-2 inline-block text-[13px] text-brand underline-offset-2 hover:underline"
      >
        {t('account.playground.balance_link')}
      </a>
    </div>
  )
}
