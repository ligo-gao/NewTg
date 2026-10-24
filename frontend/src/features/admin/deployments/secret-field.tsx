import { useEffect, useMemo, useState, type ChangeEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

export interface SecretFieldProps {
  id: string
  label: string
  /** `null` = 用户显式清除；空串 = 未填写（留空 = 不更新）。 */
  value: string | null
  configured?: boolean
  hint?: string
  error?: string
  disabled?: boolean
  /**
   * 设置页的 API Key 显示「已配置」徽章与清除按钮；创建/更新抽屉里的
   * 单次秘密（仓库密钥、秘密环境变量）不显示徽章，也不需要清除确认。
   */
  showBadge?: boolean
  /** 清除需要页面层二次确认时传回调；不传则直接置 `null`。 */
  onRequestClear?: () => void
  onChange: (value: string | null) => void
}

const SHOW_DURATION_MS = 30_000

/**
 * 秘密输入（UI §3 / §6.13）：服务器永不回显已存秘密，因此输入框始终从
 * 空开始；只有用户输入（或显式清除）才进入请求体。
 *
 * 「blur 或 30s 自动回密文」：眼睛按钮是 `aria-pressed` 的切换；一旦明文
 * 展示就开始 30 秒计时，到期或失焦都会回到密码态，避免管理员离开工位时
 * 屏幕上长期停留明文。
 */
export function SecretField({
  id,
  label,
  value,
  configured = false,
  hint,
  error,
  disabled = false,
  showBadge = true,
  onRequestClear,
  onChange,
}: SecretFieldProps) {
  const { t } = useI18n()
  const [visible, setVisible] = useState(false)
  const text = value === null ? '' : value
  const clearRequested = value === null
  const hintId = `${id}-hint`
  const errorId = `${id}-error`

  useEffect(() => {
    if (!visible) return
    const timer = globalThis.setTimeout(() => setVisible(false), SHOW_DURATION_MS)
    return () => globalThis.clearTimeout(timer)
  }, [visible])

  // 只在提示真正渲染时引用它；错误出现时提示段落会消失，否则
  // aria-describedby 会指向不存在的 id。
  const describedBy =
    error !== undefined ? errorId : hint !== undefined ? hintId : undefined

  const badge = useMemo(() => {
    if (!showBadge) return null
    return (
      <span className="flex items-center gap-2">
        <StatusBadge
          variant={configured && !clearRequested ? 'success' : 'neutral-outline'}
        >
          {configured && !clearRequested
            ? t('admin.deployments.secret.configured')
            : t('admin.deployments.secret.not_configured')}
        </StatusBadge>
        {configured && !clearRequested && !disabled ? (
          <button
            type="button"
            className="text-meta text-danger underline"
            onClick={() => {
              if (onRequestClear === undefined) onChange(null)
              else onRequestClear()
            }}
          >
            {t('admin.deployments.secret.clear')}
          </button>
        ) : null}
      </span>
    )
  }, [clearRequested, configured, disabled, onChange, onRequestClear, showBadge, t])

  const inputClasses =
    'w-full rounded-lg border bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50 ' +
    (error !== undefined ? 'border-danger' : 'border-border-default')

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between gap-3">
        <label htmlFor={id} className="text-meta font-medium text-ink-1">
          {label}
        </label>
        {badge}
      </div>
      <div className="relative">
        <input
          id={id}
          type={visible ? 'text' : 'password'}
          value={text}
          disabled={disabled}
          autoComplete="off"
          aria-invalid={error !== undefined ? true : undefined}
          aria-describedby={describedBy}
          aria-errormessage={error !== undefined ? errorId : undefined}
          placeholder={
            configured && !clearRequested
              ? t('admin.deployments.secret.placeholder')
              : undefined
          }
          // 失焦立即回密文：即使管理员忘了再点一次眼睛。
          onBlur={() => setVisible(false)}
          onChange={(event: ChangeEvent<HTMLInputElement>) =>
            onChange(event.target.value)
          }
          className={`${inputClasses} h-10 pr-11`}
        />
        <button
          type="button"
          aria-label={
            visible
              ? t('admin.deployments.secret.hide')
              : t('admin.deployments.secret.show')
          }
          aria-pressed={visible}
          disabled={disabled}
          onClick={() => setVisible((current) => !current)}
          className="absolute inset-y-0 right-0 flex w-10 items-center justify-center text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
        >
          {visible ? '◎' : '◉'}
        </button>
      </div>
      {error === undefined ? (
        <p id={hintId} className="text-meta text-ink-3">
          {hint ?? t('admin.deployments.secret.hint')}
        </p>
      ) : null}
      {error !== undefined ? (
        <p id={errorId} className="text-meta text-danger">
          {error}
        </p>
      ) : null}
    </div>
  )
}
