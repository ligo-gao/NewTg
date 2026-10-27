/**
 * 全球性能测评详情弹窗。
 *
 * 结构对齐设计稿：标题（模型名 + 副标题）→ 5 个测评类别（每类若干指标
 * 卡片）→ 模型信息 → 数据来源说明。所有文案走 i18n；数据本身来自静态
 * 样例，弹窗底部显式说明这一点（`benchmark.sample_note`）。
 *
 * 可访问性：`role="dialog"` + `aria-modal`；打开时锁定滚动并把焦点移到
 * 关闭按钮，关闭时还给触发行；ESC 与点击遮罩都能关闭。
 */

import { useEffect, useRef } from 'react'
import type { MouseEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import type { ModelBenchmark } from './benchmarks'

export interface BenchmarkModalProps {
  isOpen: boolean
  data: ModelBenchmark | null
  onClose: () => void
}

function CloseIcon() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M12 4L4 12M4 4l8 8" />
    </svg>
  )
}

export function BenchmarkModal({ isOpen, data, onClose }: BenchmarkModalProps) {
  const { t } = useI18n()
  const overlayRef = useRef<HTMLDivElement>(null)
  const closeButtonRef = useRef<HTMLButtonElement>(null)
  const restoreFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    if (!isOpen) return
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', handleEscape)
    return () => document.removeEventListener('keydown', handleEscape)
  }, [isOpen, onClose])

  useEffect(() => {
    if (isOpen) {
      restoreFocusRef.current =
        document.activeElement instanceof HTMLElement
          ? document.activeElement
          : null
      document.body.style.overflow = 'hidden'
      closeButtonRef.current?.focus()
    } else {
      document.body.style.overflow = ''
      restoreFocusRef.current?.focus()
      restoreFocusRef.current = null
    }
    return () => {
      document.body.style.overflow = ''
    }
  }, [isOpen])

  const handleOverlayClick = (event: MouseEvent<HTMLDivElement>) => {
    if (event.target === overlayRef.current) onClose()
  }

  // 弹窗内容在关闭动画/卸载时仍可安全渲染空态。
  if (!isOpen || data === null) return null

  const percentLabel = (benchmark: (typeof data.categories)[number]['benchmarks'][number]) => {
    if (benchmark.rank !== undefined) {
      return t('public.rankings.benchmark.rank_no', { rank: benchmark.rank })
    }
    if (benchmark.topPercent !== undefined) {
      return t('public.rankings.benchmark.top_percent', {
        percent: benchmark.topPercent,
      })
    }
    return null
  }

  return (
    <div
      ref={overlayRef}
      className="benchmark-modal-overlay"
      onClick={handleOverlayClick}
      role="dialog"
      aria-modal="true"
      aria-labelledby="benchmark-modal-title"
    >
      <div className="benchmark-modal-content">
        <div className="benchmark-modal-header">
          <div className="benchmark-modal-title-group">
            <h3 id="benchmark-modal-title">{data.name}</h3>
            <p className="benchmark-modal-subtitle">
              {t('public.rankings.benchmark.detail_subtitle')}
            </p>
          </div>
          <button
            ref={closeButtonRef}
            type="button"
            className="benchmark-modal-close"
            onClick={onClose}
            aria-label={t('public.rankings.benchmark.close')}
          >
            <CloseIcon />
          </button>
        </div>

        <div className="benchmark-modal-body">
          {data.categories.map((category) => (
            <div key={category.titleKey} className="benchmark-category">
              <div className="benchmark-category-title">
                {t(category.titleKey)}
              </div>
              <div className="benchmark-grid">
                {category.benchmarks.map((benchmark) => {
                  const percentile = percentLabel(benchmark)
                  return (
                    <div
                      key={benchmark.name}
                      className={`benchmark-card${
                        benchmark.highlight === true ? ' highlight' : ''
                      }`}
                    >
                      <span className="benchmark-name">{benchmark.name}</span>
                      <span className="benchmark-score">{benchmark.score}</span>
                      {percentile === null ? null : (
                        <span className="benchmark-percentile">
                          {percentile}
                          {benchmark.tagKey === undefined
                            ? null
                            : ` · ${t(benchmark.tagKey)}`}
                        </span>
                      )}
                    </div>
                  )
                })}
              </div>
            </div>
          ))}

          <div className="benchmark-detail-section">
            <h4>{t('public.rankings.benchmark.model_info')}</h4>
            <ul className="benchmark-detail-list">
              <li className="benchmark-detail-item">
                <span className="benchmark-detail-label">
                  {t('public.rankings.benchmark.provider')}
                </span>
                <span className="benchmark-detail-value">{data.provider}</span>
              </li>
              {data.details.map((detail) => (
                <li key={detail.labelKey} className="benchmark-detail-item">
                  <span className="benchmark-detail-label">
                    {t(detail.labelKey)}
                  </span>
                  <span className="benchmark-detail-value">
                    {detail.valueKey !== undefined
                      ? t(detail.valueKey)
                      : detail.value}
                  </span>
                </li>
              ))}
            </ul>
          </div>

          <div className="benchmark-detail-section">
            <h4>{t('public.rankings.benchmark.sources')}</h4>
            <p className="benchmark-detail-note">
              {t('public.rankings.benchmark.sources_academic')}{' '}
              <a
                href="https://github.com/openai/evals"
                target="_blank"
                rel="noopener noreferrer"
              >
                OpenAI Evals
              </a>
            </p>
            <p className="benchmark-detail-note">
              {t('public.rankings.benchmark.sources_arena')}{' '}
              <a href="https://lmarena.ai/" target="_blank" rel="noopener noreferrer">
                lmarena.ai
              </a>
            </p>
            <p className="benchmark-detail-note">
              {t('public.rankings.benchmark.sources_chinese')}
            </p>
            <p className="benchmark-detail-note benchmark-detail-sample">
              {t('public.rankings.benchmark.sample_note')}
            </p>
          </div>
        </div>
      </div>
    </div>
  )
}
