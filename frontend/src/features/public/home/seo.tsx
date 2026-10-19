import { useEffect } from 'react'

import type { PublicSite } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'

const DEFAULT_TITLE = 'NewTG'

function safeHttpUrl(raw: string | undefined): string | null {
  const value = (raw ?? '').trim()
  return /^https?:\/\//i.test(value) ? value : null
}

function upsertMeta(attribute: 'name' | 'property', key: string, content: string | null) {
  const selector = `meta[${attribute}="${key}"]`
  const existing = document.head.querySelector<HTMLMetaElement>(selector)
  if (content === null || content === '') {
    existing?.remove()
    return
  }
  if (existing !== null) {
    existing.setAttribute('content', content)
    return
  }
  const element = document.createElement('meta')
  element.setAttribute(attribute, key)
  element.setAttribute('content', content)
  document.head.appendChild(element)
}

function upsertCanonical(href: string | null) {
  const existing = document.head.querySelector<HTMLLinkElement>('link[rel="canonical"]')
  if (href === null) {
    existing?.remove()
    return
  }
  if (existing !== null) {
    existing.setAttribute('href', href)
    return
  }
  const link = document.createElement('link')
  link.setAttribute('rel', 'canonical')
  link.setAttribute('href', href)
  document.head.appendChild(link)
}

export interface PublicSeoProps {
  site?: PublicSite
}

/**
 * F-09-06 SEO / Open Graph for the homepage.
 *
 * The SPA has no SSR, so this hook writes the same tags a crawler would see.
 * Title precedence: `site.title` (operator branding) with `NewTG` fallback;
 * the description/OG image fall back to `site.seo.*`, then to the i18n default
 * copy and `site.logo_url`.
 *
 * The title observer exists because the i18n provider owns `document.title`
 * for the console (web-00 §7 "TC-C-02") and re-writes it on every locale
 * change. Page-level head must win while this page is mounted; a
 * MutationObserver re-asserts the site title instead of forking the provider
 * for one page. It disconnects on unmount, leaving the provider in charge
 * again.
 *
 * Custom-HTML takeover does not change any of this (F-09-06 分支流程 2).
 */
export function PublicSeo({ site }: PublicSeoProps) {
  const { t, locale } = useI18n()
  const title = site?.title?.trim() || DEFAULT_TITLE
  const description =
    site?.seo?.description?.trim() || t('public.home.seo.description')
  const image =
    safeHttpUrl(site?.seo?.og_image_url) ?? safeHttpUrl(site?.logo_url) ?? null
  const origin = safeHttpUrl(site?.server_address)
  const canonical = origin === null ? null : `${origin.replace(/\/+$/, '')}/`

  useEffect(() => {
    document.title = title
    upsertMeta('name', 'description', description)
    upsertMeta('property', 'og:title', title)
    upsertMeta('property', 'og:description', description)
    upsertMeta('property', 'og:type', 'website')
    upsertMeta('property', 'og:image', image)
    upsertMeta('property', 'og:url', canonical)
    upsertMeta('name', 'twitter:card', 'summary_large_image')
    upsertMeta('name', 'twitter:title', title)
    upsertMeta('name', 'twitter:description', description)
    upsertMeta('name', 'twitter:image', image)
    upsertCanonical(canonical)

    // `document.title` lives on <head>; observe the title element directly so
    // unrelated head updates (meta writes above) do not re-trigger this.
    const titleElement = document.head.querySelector('title')
    if (titleElement === null) return
    const observer = new MutationObserver(() => {
      if (document.title !== title) document.title = title
    })
    observer.observe(titleElement, { childList: true, characterData: true, subtree: true })
    return () => observer.disconnect()
  }, [title, description, image, canonical, locale])

  return null
}
