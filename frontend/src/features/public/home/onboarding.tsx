import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

import { Copy01Icon } from '../public-icons'

const PLACEHOLDER_BASE_URL = 'https://your-gateway.example.com/v1'
const TOKEN_PLACEHOLDER = '<YOUR_TOKEN>'

export interface BaseUrlResult {
  url: string
  /** True when `site.server_address` is missing or invalid. */
  placeholder: boolean
}

/**
 * Base URL derivation (F-09-05 step 2): trim trailing slashes first, then add
 * exactly one `/v1`. A missing/invalid `server_address` falls back to the
 * documented placeholder and the UI shows a grey hint instead of blocking.
 */
export function buildBaseUrl(serverAddress: string | undefined): BaseUrlResult {
  const raw = (serverAddress ?? '').trim().replace(/\/+$/, '')
  if (!/^https?:\/\//i.test(raw)) {
    return { url: PLACEHOLDER_BASE_URL, placeholder: true }
  }
  return { url: `${raw}/v1`, placeholder: false }
}

type SampleLang = 'curl' | 'python' | 'node'

const SAMPLE_LANGS: readonly SampleLang[] = ['curl', 'python', 'node']

/**
 * Code samples are assembled from the live base URL but the credential slot is
 * always `TOKEN_PLACEHOLDER`. The literal never contains a real token, so a
 * source scan (TC-F-09-05-02) passes by construction.
 */
export function sampleCode(lang: SampleLang, baseUrl: string): string {
  if (lang === 'curl') {
    return [
      `curl ${baseUrl}/chat/completions \\`,
      `  -H "Authorization: Bearer ${TOKEN_PLACEHOLDER}" \\`,
      '  -H "Content-Type: application/json" \\',
      '  -d \'{',
      '    "model": "gpt-4o-mini",',
      '    "messages": [{"role": "user", "content": "Hello"}],',
      '    "stream": true',
      "  }'",
    ].join('\n')
  }
  if (lang === 'python') {
    return [
      'from openai import OpenAI',
      `client = OpenAI(base_url="${baseUrl}", api_key="${TOKEN_PLACEHOLDER}")`,
      'resp = client.chat.completions.create(',
      '    model="gpt-4o-mini",',
      '    messages=[{"role": "user", "content": "Hello"}],',
      ')',
      'print(resp.choices[0].message.content)',
    ].join('\n')
  }
  return [
    'import OpenAI from "openai";',
    `const client = new OpenAI({ baseURL: "${baseUrl}", apiKey: "${TOKEN_PLACEHOLDER}" });`,
    'const resp = await client.chat.completions.create({',
    '  model: "gpt-4o-mini",',
    '  messages: [{ role: "user", content: "Hello" }],',
    '});',
    'console.log(resp.choices[0].message.content);',
  ].join('\n')
}

export interface OnboardingSectionProps {
  serverAddress?: string
  isLoggedIn: boolean
}

/**
 * F-09-05 three-step onboarding. Step 1 links to sign-up with a sanitized
 * relative redirect for guests; step 2 shows the copyable base URL; step 3
 * tabs between three snippets.
 *
 * Clipboard writes are best-effort: on a non-HTTPS origin (or a denied
 * permission) the copy API is unavailable, so the code block stays selectable
 * and a "copy manually" toast replaces the success toast — never a dead button.
 */
export function OnboardingSection({ serverAddress, isLoggedIn }: OnboardingSectionProps) {
  const { t } = useI18n()
  const [lang, setLang] = useState<SampleLang>('curl')
  const base = buildBaseUrl(serverAddress)
  const code = sampleCode(lang, base.url)
  const createTokenHref = isLoggedIn
    ? '/tokens'
    : `/sign-up?redirect=${encodeURIComponent('/tokens')}`

  async function copy(value: string) {
    try {
      if (navigator.clipboard?.writeText === undefined) throw new Error('unavailable')
      await navigator.clipboard.writeText(value)
      useToastStore.getState().push({ tone: 'success', message: t('public.home.onboarding.copied') })
    } catch {
      useToastStore
        .getState()
        .push({ tone: 'info', message: t('public.home.onboarding.copy_manual') })
    }
  }

  return (
    <section className="bg-surface-card">
      <div className="mx-auto w-full max-w-[1200px] px-4 py-16 md:px-6 md:py-20">
        <h2 className="text-base font-semibold leading-6 text-ink-1">
          {t('public.home.onboarding.title')}
        </h2>

        <ol className="mt-8 grid gap-8 lg:grid-cols-3">
          <li>
            <span className="text-xs font-medium text-ink-3">
              {t('public.home.onboarding.step', { number: 1 })}
            </span>
            <h3 className="mt-1 text-[15px] font-semibold text-ink-1">
              {t('public.home.onboarding.token_title')}
            </h3>
            <p className="mt-1 text-sm leading-[22px] text-ink-2">
              {t('public.home.onboarding.token_body')}
            </p>
            <a
              href={createTokenHref}
              className="mt-4 inline-flex h-9 items-center rounded-lg bg-brand px-3.5 text-sm font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.home.onboarding.create_token')}
            </a>
          </li>

          <li>
            <span className="text-xs font-medium text-ink-3">
              {t('public.home.onboarding.step', { number: 2 })}
            </span>
            <h3 className="mt-1 text-[15px] font-semibold text-ink-1">
              {t('public.home.onboarding.base_url_title')}
            </h3>
            <p className="mt-1 text-sm leading-[22px] text-ink-2">
              {t('public.home.onboarding.base_url_body')}
            </p>
            <div className="mt-4 flex items-center gap-2">
              <code className="min-w-0 flex-1 overflow-x-auto rounded-lg border border-border-default bg-surface-subtle px-3 py-2 font-mono text-[13px] text-ink-1">
                {base.url}
              </code>
              <button
                type="button"
                aria-label={t('public.home.onboarding.copy_base_url')}
                onClick={() => void copy(base.url)}
                className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg border border-border-default text-ink-2 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                <Copy01Icon size={16} strokeWidth={1.5} />
              </button>
            </div>
            {base.placeholder ? (
              <p className="mt-2 text-xs text-ink-3">
                {t('public.home.onboarding.base_url_unset')}
              </p>
            ) : null}
          </li>

          <li>
            <span className="text-xs font-medium text-ink-3">
              {t('public.home.onboarding.step', { number: 3 })}
            </span>
            <h3 className="mt-1 text-[15px] font-semibold text-ink-1">
              {t('public.home.onboarding.request_title')}
            </h3>
            <p className="mt-1 text-sm leading-[22px] text-ink-2">
              {t('public.home.onboarding.request_body')}
            </p>
            <div className="mt-4">
              <div role="tablist" aria-label={t('public.home.onboarding.lang_label')} className="flex gap-1">
                {SAMPLE_LANGS.map((item) => (
                  <button
                    key={item}
                    type="button"
                    role="tab"
                    aria-selected={lang === item}
                    onClick={() => setLang(item)}
                    className={[
                      'h-8 rounded-lg px-2.5 text-[13px]',
                      lang === item
                        ? 'bg-surface-hover font-medium text-ink-1'
                        : 'text-ink-2 hover:text-ink-1',
                    ].join(' ')}
                  >
                    {t(`public.home.onboarding.lang.${item}`)}
                  </button>
                ))}
              </div>
              <div className="mt-2 rounded-lg border border-border-default bg-surface-subtle">
                <pre className="overflow-x-auto p-3 font-mono text-[13px] leading-5 text-ink-1">
                  <code className="select-text">{code}</code>
                </pre>
                <div className="flex justify-end border-t border-border-hairline p-2">
                  <button
                    type="button"
                    onClick={() => void copy(code)}
                    className="inline-flex h-8 items-center gap-1.5 rounded-lg px-2.5 text-[13px] text-ink-2 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  >
                    <Copy01Icon size={14} strokeWidth={1.5} />
                    {t('public.home.onboarding.copy_code')}
                  </button>
                </div>
              </div>
            </div>
          </li>
        </ol>
      </div>
    </section>
  )
}
