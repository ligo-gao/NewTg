import { useState, type ReactNode } from 'react'

import { writeClipboard } from '@/features/tokens/clipboard'
import { useI18n } from '@/shared/lib/i18n-context'

interface Block {
  kind: 'text' | 'code'
  language?: string
  text: string
}

/**
 * 把 Markdown 切成「普通文本 / 围栏代码块」两类块。
 *
 * 为什么不引入 Markdown 库：任务禁止新增依赖，而 Playground 只需要安全地
 * 呈现模型输出。这里**从不使用 `dangerouslySetInnerHTML`**——模型输出是不可
 * 信输入，任何 HTML 都按纯文本渲染，只有围栏代码块得到等宽 + 复制按钮。
 */
export function parseBlocks(source: string): Block[] {
  const lines = source.split('\n')
  const blocks: Block[] = []
  let textBuffer: string[] = []
  let codeBuffer: string[] | null = null
  let language: string | undefined

  function flushText(): void {
    if (textBuffer.length === 0) return
    blocks.push({ kind: 'text', text: textBuffer.join('\n') })
    textBuffer = []
  }

  for (const line of lines) {
    if (codeBuffer === null) {
      const fence = /^```([\w-]*)\s*$/.exec(line)
      if (fence !== null) {
        flushText()
        codeBuffer = []
        language = fence[1] === '' ? undefined : fence[1]
      } else {
        textBuffer.push(line)
      }
    } else if (/^```\s*$/.test(line)) {
      blocks.push({
        kind: 'code',
        ...(language === undefined ? {} : { language }),
        text: codeBuffer.join('\n'),
      })
      codeBuffer = null
      language = undefined
    } else {
      codeBuffer.push(line)
    }
  }
  if (codeBuffer !== null) {
    // 未闭合的围栏（流式中途）也按代码块呈现，保证已生成内容不丢。
    blocks.push({
      kind: 'code',
      ...(language === undefined ? {} : { language }),
      text: codeBuffer.join('\n'),
    })
  }
  flushText()
  return blocks
}

/** 行内代码 `` `x` `` 渲染；其余文本原样保留空白。 */
function renderInline(text: string, keyPrefix: string): ReactNode[] {
  const parts = text.split(/(`[^`\n]+`)/g)
  return parts.map((part, index) => {
    if (part.startsWith('`') && part.endsWith('`') && part.length > 2) {
      return (
        <code
          key={`${keyPrefix}-${index}`}
          className="rounded bg-surface-subtle px-1 py-0.5 font-mono text-[0.9em]"
        >
          {part.slice(1, -1)}
        </code>
      )
    }
    return <span key={`${keyPrefix}-${index}`}>{part}</span>
  })
}

export function MessageContent({ text }: { text: string }) {
  const { t } = useI18n()
  const blocks = parseBlocks(text)

  return (
    <div className="space-y-2 whitespace-pre-wrap break-words text-sm text-ink-1">
      {blocks.map((block, index) =>
        block.kind === 'code' ? (
          <CodeBlock
            key={index}
            text={block.text}
            language={block.language}
            copyLabel={t('account.playground.copy_code')}
            copiedLabel={t('account.playground.copied')}
          />
        ) : (
          <p key={index}>{renderInline(block.text, `text-${index}`)}</p>
        ),
      )}
    </div>
  )
}

function CodeBlock({
  text,
  language,
  copyLabel,
  copiedLabel,
}: {
  text: string
  language?: string
  copyLabel: string
  copiedLabel: string
}) {
  const [copied, setCopied] = useState(false)

  return (
    <div className="relative rounded-lg border border-border-hairline bg-surface-subtle">
      <div className="flex items-center justify-between border-b border-border-hairline px-3 py-1 text-[11px] text-ink-3">
        <span>{language ?? ''}</span>
        <button
          type="button"
          onClick={() => {
            void writeClipboard(text).then((ok) => setCopied(ok))
          }}
          className="rounded px-1.5 py-0.5 hover:bg-surface-hover hover:text-ink-1"
        >
          {copied ? copiedLabel : copyLabel}
        </button>
      </div>
      <pre className="overflow-x-auto px-3 py-2 font-mono text-xs text-ink-1">
        <code>{text}</code>
      </pre>
    </div>
  )
}
