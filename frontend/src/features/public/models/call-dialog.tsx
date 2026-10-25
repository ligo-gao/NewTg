/**
 * 模型库「立即调用」弹框。
 *
 * 结构对齐 demo 的三步：创建 Key → 发送请求 → 读取响应。第一、二步都只
 * 展示本站的 OpenAI 兼容端点；第三步的响应是固定片段，不发起真实请求。
 * 密钥一律是占位符，base URL 只来自 `serverAddress`。
 */

import { useId, useState } from 'react'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { sampleTarget } from '@/features/public/pricing/code-samples'
import { copyText } from '@/features/public/pricing/copy'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleTabListKeyDown } from '@/shared/lib/roving-focus'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

import type { CallKind } from './performance'
import { callSample, callSpec, type CallTab } from './samples'

export interface CallDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  model: PublicModelCard
  kind: CallKind
  serverAddress: string | undefined
  isLoggedIn: boolean
}

const TABS: ReadonlyArray<{ tab: CallTab; labelKey: string }> = [
  { tab: 'curl', labelKey: 'public.models.call.tab_curl' },
  { tab: 'python', labelKey: 'public.models.call.tab_python' },
  { tab: 'node', labelKey: 'public.models.call.tab_node' },
]

export function CallDialog({
  open,
  onOpenChange,
  model,
  kind,
  serverAddress,
  isLoggedIn,
}: CallDialogProps) {
  const { t } = useI18n()
  const [tab, setTab] = useState<CallTab>('curl')
  const [stream, setStream] = useState(false)
  // demo 口径：每次打开都回到 cURL + 非流式。重置放在渲染期（state during
  // render）而不是 effect：effect 在首帧之后才跑，重开时会先把上一个模型
  // 的 Python/流式状态画出来一帧。
  const [wasOpen, setWasOpen] = useState(open)
  const [openedModel, setOpenedModel] = useState(model.model_id)
  if (open && (!wasOpen || openedModel !== model.model_id)) {
    setWasOpen(true)
    setOpenedModel(model.model_id)
    setTab('curl')
    setStream(false)
  } else if (!open && wasOpen) {
    setWasOpen(false)
  }
  const panelId = useId()
  const push = useToastStore((state) => state.push)
  const heading = t('public.models.call.title', {
    name: model.display_name || model.model_id,
  })

  const spec = callSpec(kind, model.model_id, kind === 'chat' && stream)
  const target = sampleTarget(serverAddress)
  const code = callSample(tab, spec, target)

  const copy = async (text: string) => {
    const ok = await copyText(text)
    push({
      tone: ok ? 'success' : 'error',
      message: ok
        ? t('public.models.call.copied')
        : t('public.pricing.copy_failed'),
    })
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={heading}
      description={`${model.model_id} · ${spec.method} ${spec.path}`}
      panelClassName="max-w-none! xl:max-w-[880px]!"
      closeLabel={t('public.models.call.close')}
    >
      {/* Dialog 自身不滚动内容；这里给正文一个高度上限，短屏下标题与关闭
          按钮保持固定，三步内容在正文内滚动，最后一步始终可达。 */}
      <div
        data-testid="call-dialog-scroll"
        className="max-h-[72vh] overflow-y-auto pr-1"
      >
        <ol className="flex flex-col gap-5">
        <li className="flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <span
              aria-hidden="true"
              className="inline-flex size-6 shrink-0 items-center justify-center rounded-full bg-surface-subtle text-badge text-ink-2"
            >
              1
            </span>
            <h3 className="text-card-title text-ink-1">
              {t('public.models.call.step_create')}
            </h3>
          </div>
          <p className="text-meta text-ink-2">
            {t('public.models.call.step_create_body')}
          </p>
          <a
            href={isLoggedIn ? '/tokens' : '/sign-in?redirect=%2Ftokens'}
            className="inline-flex w-fit items-center rounded-md border border-border-hairline px-3 py-1 text-meta font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {isLoggedIn
              ? t('public.models.call.open_tokens')
              : t('public.models.call.sign_in')}
          </a>
        </li>

        <li className="flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <span
              aria-hidden="true"
              className="inline-flex size-6 shrink-0 items-center justify-center rounded-full bg-surface-subtle text-badge text-ink-2"
            >
              2
            </span>
            <h3 className="text-card-title text-ink-1">
              {t('public.models.call.step_send')}
            </h3>
          </div>

          <div className="flex flex-wrap items-center justify-between gap-2">
            <div
              role="tablist"
              aria-label={heading}
              onKeyDown={handleTabListKeyDown}
              className="inline-flex items-center gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-1"
            >
              {TABS.map((option) => {
                const selected = option.tab === tab
                return (
                  <button
                    key={option.tab}
                    type="button"
                    role="tab"
                    id={`${panelId}-${option.tab}`}
                    aria-selected={selected}
                    aria-controls={`${panelId}-panel`}
                    tabIndex={selected ? 0 : -1}
                    onClick={() => setTab(option.tab)}
                    className={[
                      'rounded-md px-3 py-1 text-meta font-medium leading-5',
                      selected
                        ? 'bg-surface-card text-ink-1 shadow-sm'
                        : 'text-ink-2 hover:text-ink-1',
                      'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    ].join(' ')}
                  >
                    {t(option.labelKey)}
                  </button>
                )
              })}
            </div>

            <div className="flex items-center gap-3">
              {kind === 'chat' ? (
                <label className="inline-flex items-center gap-2 text-meta text-ink-2">
                  <input
                    type="checkbox"
                    checked={stream}
                    onChange={(event) => setStream(event.target.checked)}
                    className="size-3.5 accent-brand"
                  />
                  {t('public.models.call.stream')}
                </label>
              ) : null}
              <button
                type="button"
                onClick={() => void copy(code)}
                className="rounded-md border border-border-hairline px-3 py-1 text-meta font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('public.models.call.copy')}
              </button>
            </div>
          </div>

          <div
            role="tabpanel"
            id={`${panelId}-panel`}
            aria-labelledby={`${panelId}-${tab}`}
            className="overflow-hidden rounded-lg border border-border-default bg-surface-card"
          >
            <pre
              tabIndex={0}
              className="overflow-x-auto p-4 text-meta leading-5 text-ink-1"
            >
              <code>{code}</code>
            </pre>
            {target.configured ? null : (
              <p className="border-t border-warning-line bg-warning-bg px-4 py-2 text-meta text-warning">
                {t('public.models.call.server_unset')}
              </p>
            )}
          </div>
        </li>

        <li className="flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <span
              aria-hidden="true"
              className="inline-flex size-6 shrink-0 items-center justify-center rounded-full bg-surface-subtle text-badge text-ink-2"
            >
              3
            </span>
            <h3 className="text-card-title text-ink-1">
              {t('public.models.call.step_response')}
            </h3>
          </div>
          <pre
            tabIndex={0}
            className="overflow-x-auto whitespace-pre-wrap rounded-lg border border-border-default bg-surface-subtle p-4 font-mono text-meta leading-5 text-ink-1"
          >
            <code>{spec.response}</code>
          </pre>
        </li>
        </ol>

        <p className="mt-4 text-meta text-ink-3">
          {t('public.models.call.note')}
        </p>
      </div>
    </Dialog>
  )
}
