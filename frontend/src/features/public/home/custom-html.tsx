import { useEffect, useRef } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Client-side allowlist for `site.home_custom_html` / `site.footer_html`.
 *
 * The backend sanitizes both before they are stored (modules-08 F-08-02), so
 * this is defence in depth, not the primary control. Two reasons it still
 * exists:
 *   1. `dangerouslySetInnerHTML` would otherwise be one server regression away
 *      from executing script in every visitor's session;
 *   2. the sanitizer's *output* decides the documented fallback — an empty
 *      result means "render the five default sections / default footer links"
 *      (F-09-06 分支流程 4, F-09-37 状态设计).
 *
 * Allowlist mirrors the server's documented set (`a/p/br/strong/em`); the few
 * extra list/heading tags are kept because operator-authored footer copy
 * routinely uses them and they carry no script surface. Event attributes are
 * dropped implicitly: only the attributes read below are copied to the output.
 */
const ALLOWED_TAGS = new Set([
  'A',
  'P',
  'BR',
  'STRONG',
  'EM',
  'B',
  'I',
  'UL',
  'OL',
  'LI',
  // The module spec's own custom-HTML example is `<h1>…</h1>`; headings carry
  // no script surface and operators need them for document structure.
  'H1',
  'H2',
  'H3',
])

/** Removed together with their content (unwrapping would leak CSS/JS text). */
const DROPPED_TAGS = new Set([
  'SCRIPT',
  'STYLE',
  'IFRAME',
  'OBJECT',
  'EMBED',
  'LINK',
  'META',
  'FORM',
  'INPUT',
  'TEXTAREA',
  'SELECT',
  'SVG',
])

function safeHref(raw: string): string | null {
  const value = raw.trim()
  // Same rule as the backend URL validation (F-09-38): absolute http(s) or a
  // site-relative path. Everything else (`javascript:`, `data:`, protocol
  // relative `//host`) is discarded rather than rewritten.
  if (value.startsWith('/') && !value.startsWith('//')) return value
  if (/^https?:\/\//i.test(value)) return value
  return null
}

function sanitizeNode(node: Node, doc: Document): Node[] {
  if (node.nodeType === Node.TEXT_NODE) {
    return [doc.createTextNode(node.textContent ?? '')]
  }
  if (node.nodeType !== Node.ELEMENT_NODE) return []

  const element = node as Element
  const tag = element.tagName.toUpperCase()
  if (DROPPED_TAGS.has(tag)) return []

  const children: Node[] = []
  element.childNodes.forEach((child) => children.push(...sanitizeNode(child, doc)))

  if (!ALLOWED_TAGS.has(tag)) {
    // Unknown-but-harmless markup is unwrapped; its text still renders.
    return children
  }

  const clean = doc.createElement(tag.toLowerCase())
  if (tag === 'A') {
    const href = safeHref(element.getAttribute('href') ?? '')
    if (href !== null) {
      clean.setAttribute('href', href)
      // External links always open safely; relative links stay in place.
      if (/^https?:\/\//i.test(href)) {
        clean.setAttribute('target', '_blank')
        clean.setAttribute('rel', 'noopener noreferrer')
      }
    }
  }
  children.forEach((child) => clean.appendChild(child))
  return [clean]
}

/**
 * Returns sanitized HTML, or `''` when nothing survived the allowlist. An
 * empty input stays empty so callers can use one fallback branch.
 */
export function sanitizeHtmlFragment(html: string | undefined): string {
  if (!html || html.trim() === '') return ''
  if (typeof DOMParser === 'undefined') return ''
  const parsed = new DOMParser().parseFromString(html, 'text/html')
  const container = parsed.createElement('div')
  parsed.body.childNodes.forEach((child) => {
    sanitizeNode(child, parsed).forEach((node) => container.appendChild(node))
  })
  return container.innerHTML
}

/** Plain-text摘要 used by the footer brand column (never injected as HTML). */
export function htmlToPlainText(html: string | undefined, limit = 160): string {
  if (!html) return ''
  const text = html
    .replace(/<(script|style)[\s\S]*?<\/\1>/gi, ' ')
    .replace(/<[^>]*>/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  return text.length > limit ? `${text.slice(0, limit - 1).trimEnd()}…` : text
}

export interface CustomHomeHtmlProps {
  html: string
}

/**
 * F-09-06 custom homepage takeover: the five built-in sections are replaced by
 * operator HTML while header, footer and SEO stay in place. Rendered through
 * `dangerouslySetInnerHTML` after the client allowlist above — the same choice
 * the module spec makes, since a restricted iframe would break in-page anchors
 * and inherit no theme tokens.
 */
export function CustomHomeHtml({ html }: CustomHomeHtmlProps) {
  const { t } = useI18n()
  const ref = useRef<HTMLDivElement>(null)
  const safe = sanitizeHtmlFragment(html)

  // Anchors that survive sanitization already carry rel/target. This effect is
  // the belt-and-braces pass for server HTML rendered before React hydration
  // and keeps the "no window.opener" guarantee if the server allowlist changes.
  useEffect(() => {
    ref.current?.querySelectorAll('a').forEach((anchor) => {
      const href = anchor.getAttribute('href') ?? ''
      if (/^https?:\/\//i.test(href)) {
        anchor.setAttribute('target', '_blank')
        anchor.setAttribute('rel', 'noopener noreferrer')
      }
    })
  }, [safe])

  return (
    <section
      aria-label={t('public.home.custom_section')}
      className="mx-auto w-full max-w-[1200px] px-4 py-12 md:px-6 md:py-16"
    >
      <div
        ref={ref}
        className="text-sm leading-[22px] text-ink-2 [&_a]:text-brand [&_a:hover]:underline [&_h1]:text-2xl [&_h1]:font-semibold [&_h1]:text-ink-1 [&_p]:mt-3 [&_strong]:font-semibold [&_strong]:text-ink-1"
        dangerouslySetInnerHTML={{ __html: safe }}
      />
    </section>
  )
}
