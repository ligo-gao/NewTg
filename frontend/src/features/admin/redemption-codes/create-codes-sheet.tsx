import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import {
  downloadRedemptionCodes,
  REDEMPTION_EXPORT_LIMIT,
  type SaveFile,
} from './redemption-export'

type CreateRequest = components['schemas']['RedemptionCodeCreateRequest']
type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']

interface FormValues {
  batchName: string
  credits: string
  count: string
  /** `datetime-local` 的本地值；空串 = 永不过期。 */
  expiresAt: string
}

type FieldErrors = Partial<Record<'batch_name' | 'credits' | 'count' | 'expires_at', string>>

/**
 * 契约 `batch_name` 默认「批次-YYYYMMDD」（F-04-15），按本地日期生成。
 * 文案走 i18n 模板而不是硬编码 `Batch-`：默认值会原样落库并出现在列表/
 * 导出文件里，硬编码英文会让其他语种的管理员留下英文批次名。
 */
function defaultBatchName(t: TranslateFn, now: Date = new Date()): string {
  const stamp = [
    now.getFullYear(),
    String(now.getMonth() + 1).padStart(2, '0'),
    String(now.getDate()).padStart(2, '0'),
  ].join('')
  return t('admin.redemption.create.default_batch', { date: stamp })
}

/**
 * 服务端 400 的 `detail` 是内部英文（`billing: invalid redemption batch:
 * count must be between ...`），契约也没有 `fields`。这里按前缀映射回
 * 对应的本地化字段文案；认不出的细节一律落到通用失败文案，绝不把内部
 * 英文直接展示给用户。
 */
function validationMessage(detail: string | undefined, t: TranslateFn): string {
  if (detail !== undefined) {
    if (detail.includes('expires_at must be')) {
      return t('admin.redemption.create.error_expires')
    }
    if (detail.includes('count must be')) {
      return t('admin.redemption.create.error_count')
    }
    if (detail.includes('credits must be')) {
      return t('admin.redemption.create.error_credits')
    }
    if (detail.includes('batch name')) {
      return t('admin.redemption.create.error_batch')
    }
  }
  return t('admin.redemption.create.error')
}

function parsePositiveInteger(value: string): number | undefined {
  if (!/^\d+$/.test(value.trim())) return undefined
  const parsed = Number(value.trim())
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : undefined
}

export interface CreateCodesSheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 创建成功后交给页面展示一次性明文弹窗。 */
  onCreated: (result: CreateResponse) => void
}

/**
 * F-04-15 批量生成抽屉（480px）。
 *
 * 前端校验只是即时反馈，服务端仍是权威；两者规则一致（count 1–100、
 * credits > 0、过期时间晚于当前），因此不会出现「前端放过、后端 400」的
 * 常见落差。生成不是危险操作，按设计不弹二次确认。
 */
export function CreateCodesSheet({ open, onOpenChange, onCreated }: CreateCodesSheetProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [values, setValues] = useState<FormValues>(() => ({
    batchName: defaultBatchName(t),
    credits: '',
    count: '10',
    expiresAt: '',
  }))
  const [errors, setErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  function reset(): void {
    setValues({ batchName: defaultBatchName(t), credits: '', count: '10', expiresAt: '' })
    setErrors({})
    setFormError(undefined)
  }

  function validate(): { errors: FieldErrors; body?: CreateRequest } {
    const next: FieldErrors = {}
    const batchName = values.batchName.trim()
    if (batchName.length < 1 || batchName.length > 20) {
      next.batch_name = t('admin.redemption.create.error_batch')
    }

    const credits = parsePositiveInteger(values.credits)
    if (credits === undefined) {
      next.credits = t('admin.redemption.create.error_credits')
    }

    const count = parsePositiveInteger(values.count)
    if (count === undefined || count > 100) {
      next.count = t('admin.redemption.create.error_count')
    }

    let expiresAt: string | null = null
    if (values.expiresAt !== '') {
      const parsed = new Date(values.expiresAt)
      // 早于（或等于）当前时间会被服务端以 EXPIRED_TIME_INVALID 拒绝；
      // 在字段上先拦住，用户不必往返一次。
      if (Number.isNaN(parsed.getTime()) || parsed.getTime() <= Date.now()) {
        next.expires_at = t('admin.redemption.create.error_expires')
      } else {
        expiresAt = parsed.toISOString()
      }
    }

    if (Object.keys(next).length > 0 || credits === undefined || count === undefined) {
      return { errors: next }
    }

    return {
      errors: next,
      body: { batch_name: batchName, credits, count, expires_at: expiresAt },
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (submitting) return

    const { errors: validationErrors, body } = validate()
    setErrors(validationErrors)
    setFormError(undefined)
    if (body === undefined) return

    setSubmitting(true)
    try {
      const result = await apiClient.POST('/api/redemption-codes', { body })
      onCreated(requireData(result))
      reset()
      onOpenChange(false)
    } catch (error) {
      if (error instanceof ApiError) {
        // 契约里创建失败只有 400（无 `fields`），服务端用 `detail` 说明
        // 哪个校验没过。按前缀映射成本地化文案，避免把内部英文抛给用户。
        setFormError(
          error.kind === 'rate_limited'
            ? t('admin.redemption.rate_limited')
            : error.kind === 'validation'
              ? validationMessage(error.detail, t)
              : t('admin.redemption.create.error'),
        )
      } else {
        setFormError(t('admin.redemption.create.error'))
      }
      // 写操作失败一律不自动重试（QueryClient 的 mutations.retry=0）：
      // 重复提交会生成第二批量兑换码，代价是真实的积分。
      push({ tone: 'error', message: t('admin.redemption.create.error') })
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Sheet
      open={open}
      onOpenChange={(nextOpen) => {
        if (!nextOpen && submitting) return
        if (!nextOpen) reset()
        onOpenChange(nextOpen)
      }}
      title={t('admin.redemption.create.title')}
      footer={
        <Button type="submit" form="redemption-create-form" disabled={submitting}>
          {submitting
            ? t('admin.redemption.create.submitting')
            : t('admin.redemption.create.submit')}
        </Button>
      }
    >
      <form id="redemption-create-form" className="flex flex-col gap-4" onSubmit={(event) => void handleSubmit(event)}>
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <TextField
          label={t('admin.redemption.create.batch')}
          hint={t('admin.redemption.create.batch_hint')}
          maxLength={20}
          value={values.batchName}
          error={errors.batch_name}
          onChange={(event) => {
            setValues((current) => ({ ...current, batchName: event.target.value }))
          }}
        />
        <TextField
          label={t('admin.redemption.create.credits')}
          inputMode="numeric"
          value={values.credits}
          error={errors.credits}
          onChange={(event) => {
            setValues((current) => ({ ...current, credits: event.target.value }))
          }}
        />
        <TextField
          label={t('admin.redemption.create.count')}
          inputMode="numeric"
          value={values.count}
          error={errors.count}
          onChange={(event) => {
            setValues((current) => ({ ...current, count: event.target.value }))
          }}
        />
        <TextField
          label={t('admin.redemption.create.expires')}
          type="datetime-local"
          value={values.expiresAt}
          error={errors.expires_at}
          onChange={(event) => {
            setValues((current) => ({ ...current, expiresAt: event.target.value }))
          }}
        />
      </form>
    </Sheet>
  )
}

export interface CreateCodesResultDialogProps {
  result: CreateResponse | null
  onClose: () => void
  /** 下载接缝（测试注入 spy）；生产环境走浏览器下载。 */
  saveFile?: SaveFile
}

/**
 * 生成成功弹窗。**明文码只在创建响应里出现一次**——服务端只存 SHA-256
 * 哈希，列表投影的 `code_mask` 固定为 `••••••`，因此这个弹窗关闭后没有任何
 * 入口能再取回码值；「复制全部」与本地下载是仅有的两种带走方式。
 *
 * 下载（F-04-17）是纯前端 Blob 生成，不依赖服务端端点；剪贴板不可用时
 * 它仍是可用的兜底通道。列表页的「导出元数据」是 P1 后置项，本页不渲染
 * 入口（见 D4 收口报告的登记项）。
 */
export function CreateCodesResultDialog({
  result,
  onClose,
  saveFile,
}: CreateCodesResultDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)

  if (result === null) return null

  // 收窄后的别名：闭包（onClick/copyAll）里继续用 `result` 会丢失非空
  // 收窄，`data` 是 const，TypeScript 会保留。
  const data = result

  // 超过上限时禁用下载并提示分批；复制仍可用（用户可能只想复制一部分）。
  const tooMany = data.codes.length > REDEMPTION_EXPORT_LIMIT

  async function copyAll(): Promise<void> {
    try {
      await navigator.clipboard.writeText(data.codes.join('\n'))
      push({ tone: 'success', message: t('admin.redemption.create.copied', { count: data.count }) })
    } catch {
      push({ tone: 'error', message: t('admin.redemption.create.copy_error') })
    }
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={t('admin.redemption.create.result_title', { count: data.count })}
      description={t('admin.redemption.create.result_warning')}
      footer={
        <>
          <Button variant="secondary" onClick={() => void copyAll()}>
            {t('admin.redemption.create.copy_all')}
          </Button>
          <Button onClick={onClose}>{t('admin.redemption.create.done')}</Button>
        </>
      }
    >
      <ul
        className="h-[240px] overflow-y-auto rounded-lg border border-border-hairline bg-surface-subtle p-3 font-mono text-[13px] tracking-[0.05em]"
        aria-label={t('admin.redemption.create.codes_label')}
      >
        {data.codes.map((code) => (
          <li key={code} className="py-0.5">
            {code}
          </li>
        ))}
      </ul>
      <p className="mt-2 text-xs text-warning">{t('admin.redemption.create.result_warning')}</p>
      <div
        className="mt-3 flex flex-wrap items-center gap-2"
        role="group"
        aria-label={t('admin.redemption.create.export_group')}
      >
        <Button
          variant="secondary"
          className="h-8 px-3 text-[13px]"
          disabled={tooMany}
          onClick={() => downloadRedemptionCodes(data, 'txt', {}, saveFile)}
        >
          {t('admin.redemption.create.export_txt')}
        </Button>
        <Button
          variant="secondary"
          className="h-8 px-3 text-[13px]"
          disabled={tooMany}
          onClick={() => downloadRedemptionCodes(data, 'markdown', {}, saveFile)}
        >
          {t('admin.redemption.create.export_markdown')}
        </Button>
      </div>
      {tooMany ? (
        <p className="mt-2 text-xs text-danger">
          {t('admin.redemption.create.export_too_many')}
        </p>
      ) : null}
    </Dialog>
  )
}
