import type { ReactNode } from 'react'

/**
 * 系统状态页的统一卡片外壳。抽出来是因为更新检查、轮询状态与 MFA 卡都是
 * 新增区块：如果每张卡各写一份边框/内边距，设计 token 一改就会出现四五处
 * 漂移（web-00 §4.8 的单一来源原则）。
 */
export function SystemCard({
  title,
  children,
  testId,
  action,
}: {
  title: string
  children: ReactNode
  testId?: string
  action?: ReactNode
}) {
  return (
    <section
      data-testid={testId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <div className="mb-3 flex items-start justify-between gap-3">
        <h2 className="text-[15px] font-semibold leading-[22px] text-ink-1">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  )
}
