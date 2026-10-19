import { useQueryClient } from '@tanstack/react-query'
import { useRef, useState, type DragEvent, type KeyboardEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  uploadTaskPlugin,
  type UploadTaskPluginResponse,
} from './api'
import { SectionCard } from './components'
import { errorMessage, issueMessages, problemSlug, shortHash } from './utils'

/**
 * 前端唯一的预检：后缀与体积（§3.10 S1）。编译、元数据与 requiredCapabilities
 * 校验全部在服务端完成，客户端不解析插件源。
 */
const MAX_BYTES = 2 * 1024 * 1024

interface SelectedSource {
  name: string
  size: number
  text: string
}

/**
 * 读取选中文件的文本。用 FileReader 而不是 `Blob.text()`：后者在部分
 * webview / 测试环境（jsdom）中缺失，而上传路径必须在浏览器与测试里同构。
 */
function readFileText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => {
      resolve(typeof reader.result === 'string' ? reader.result : '')
    }
    reader.onerror = () => {
      reject(reader.error ?? new Error('file read failed'))
    }
    reader.readAsText(file)
  })
}

export interface UploadTabProps {
  canManage: boolean
  online: boolean
}

/**
 * 上传 Tab（F-08-30）。
 *
 * 契约只有一个上传端点，且服务端在「编译校验通过」之后才入库、失败零写入，
 * 所以这里没有「先校验再安装」的第二跳：「确认安装」即校验 + 安装的同一步。
 * 编译失败只展示错误面板，不提供任何「仍然安装」入口。
 *
 * 「从 URL 导入」入口**不渲染**：该端点未实现（已在 t8 报告登记）。
 */
export function UploadTab({ canManage, online }: UploadTabProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const inputRef = useRef<HTMLInputElement>(null)
  const intentRef = useRef<string | null>(null)

  const [source, setSource] = useState<SelectedSource | null>(null)
  const [precheckError, setPrecheckError] = useState<string | null>(null)
  const [submitError, setSubmitError] = useState<{
    slug: string
    message: string
    issues: string[]
  } | null>(null)
  const [result, setResult] = useState<UploadTaskPluginResponse | null>(null)
  const [submitting, setSubmitting] = useState(false)

  function reset(): void {
    setSource(null)
    setSubmitError(null)
    setResult(null)
    setPrecheckError(null)
    intentRef.current = null
    if (inputRef.current !== null) inputRef.current.value = ''
  }

  async function pick(file: File | undefined): Promise<void> {
    if (file === undefined) return
    setResult(null)
    setSubmitError(null)
    setPrecheckError(null)
    if (!file.name.toLowerCase().endsWith('.js')) {
      setPrecheckError(t('admin.task_plugins.upload.error_suffix'))
      setSource(null)
      return
    }
    if (file.size > MAX_BYTES) {
      setPrecheckError(t('admin.task_plugins.upload.error_size'))
      setSource(null)
      return
    }
    try {
      const text = await readFileText(file)
      setSource({ name: file.name, size: file.size, text })
      intentRef.current = createIdempotencyKey()
    } catch {
      setPrecheckError(t('admin.task_plugins.upload.error_read'))
      setSource(null)
    }
  }

  async function submit(): Promise<void> {
    if (source === null) return
    setSubmitting(true)
    setSubmitError(null)
    setResult(null)
    // 同一次用户意图的重试复用同一 Idempotency-Key；成功后作废。
    const idempotencyKey = intentRef.current ?? createIdempotencyKey()
    intentRef.current = idempotencyKey
    try {
      const response = await uploadTaskPlugin(source.text, idempotencyKey)
      setResult(response)
      setSource(null)
      if (inputRef.current !== null) inputRef.current.value = ''
      intentRef.current = null
      void queryClient.invalidateQueries({ queryKey: ['plugin-list'] })
      void queryClient.invalidateQueries({ queryKey: ['plugin-runtime'] })
    } catch (error) {
      setSubmitError({
        slug: problemSlug(error),
        message: errorMessage(error),
        issues: issueMessages(error),
      })
    } finally {
      setSubmitting(false)
    }
  }

  function handleKeyDown(event: KeyboardEvent<HTMLDivElement>): void {
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    inputRef.current?.click()
  }

  function handleDrop(event: DragEvent<HTMLDivElement>): void {
    event.preventDefault()
    void pick(event.dataTransfer.files[0])
  }

  const compileFailed = submitError?.slug === 'plugin_compile_failed'
  const disabled = !canManage || !online || submitting || source === null

  return (
    <div className="space-y-3">
      <SectionCard>
        <div
          role="button"
          tabIndex={0}
          aria-label={t('admin.task_plugins.upload.dropzone_label')}
          data-testid="plugin-dropzone"
          className="flex flex-col items-center gap-2 rounded-xl border border-dashed border-border-default px-6 py-10 text-center hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          onClick={() => inputRef.current?.click()}
          onKeyDown={handleKeyDown}
          onDragOver={(event) => event.preventDefault()}
          onDrop={handleDrop}
        >
          <p className="text-sm font-medium text-ink-1">
            {t('admin.task_plugins.upload.dropzone_title')}
          </p>
          <p className="text-xs text-ink-3">
            {t('admin.task_plugins.upload.dropzone_hint')}
          </p>
          <Button
            variant="secondary"
            onClick={(event) => {
              event.stopPropagation()
              inputRef.current?.click()
            }}
          >
            {t('admin.task_plugins.upload.choose_file')}
          </Button>
          <input
            ref={inputRef}
            type="file"
            accept=".js,application/javascript,text/javascript"
            hidden
            aria-hidden="true"
            tabIndex={-1}
            data-testid="plugin-file-input"
            onChange={(event) => void pick(event.target.files?.[0])}
          />
        </div>

        {precheckError !== null ? (
          <div className="mt-3">
            <FormAlert>{precheckError}</FormAlert>
          </div>
        ) : null}

        {source !== null ? (
          <div className="mt-3 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border-default bg-surface-subtle px-3 py-2 text-sm">
            <span className="font-mono text-xs text-ink-1">
              {source.name} · {Math.ceil(source.size / 1024)} KiB
            </span>
            <span className="flex gap-2">
              <Button
                disabled={disabled}
                title={!online ? t('admin.task_plugins.common.offline') : undefined}
                onClick={() => void submit()}
              >
                {submitting
                  ? t('admin.task_plugins.upload.uploading')
                  : t('admin.task_plugins.upload.confirm_install')}
              </Button>
              <Button variant="secondary" disabled={submitting} onClick={reset}>
                {t('admin.task_plugins.common.cancel')}
              </Button>
            </span>
          </div>
        ) : (
          <p className="mt-3 text-xs text-ink-3">
            {t('admin.task_plugins.upload.empty_preview')}
          </p>
        )}

        {!online ? (
          <p className="mt-2 text-xs text-ink-3">
            {t('admin.task_plugins.common.offline')}
          </p>
        ) : null}
      </SectionCard>

      {submitError !== null ? (
        <div data-testid="plugin-compile-error" role="alert">
          {compileFailed ? (
            <div className="rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-sm text-danger">
              <p className="font-medium">
                {t('admin.task_plugins.upload.compile_failed_title', {
                  error: submitError.issues[0] ?? submitError.message,
                })}
              </p>
              {submitError.issues.length > 1
                ? submitError.issues.slice(1).map((line) => (
                    <p key={line} className="mt-1 font-mono text-xs">
                      {line}
                    </p>
                  ))
                : null}
            </div>
          ) : (
            <FormAlert>
              {submitError.slug === 'plugin_version_conflict'
                ? t('admin.task_plugins.upload.version_conflict')
                : submitError.message}
            </FormAlert>
          )}
        </div>
      ) : null}

      {result !== null ? (
        <SectionCard
          testId="plugin-upload-preview"
          title={t('admin.task_plugins.upload.preview_title')}
        >
          <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
            <div>
              <dt className="text-xs text-ink-3">
                {t('admin.task_plugins.detail.key')}
              </dt>
              <dd className="font-mono">{result.plugin.key}</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-3">
                {t('admin.task_plugins.detail.name')}
              </dt>
              <dd>{result.plugin.name}</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-3">
                {t('admin.task_plugins.detail.version')}
              </dt>
              <dd className="font-mono">{result.version.version}</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-3">
                {t('admin.task_plugins.detail.hash')}
              </dt>
              <dd className="font-mono" title={result.version.source_hash}>
                {shortHash(result.version.source_hash)}
              </dd>
            </div>
          </dl>
          <p className="mt-3 text-xs text-ink-3">
            {t('admin.task_plugins.upload.not_activated')}
          </p>
        </SectionCard>
      ) : null}
    </div>
  )
}
