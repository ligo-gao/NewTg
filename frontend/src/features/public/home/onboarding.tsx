import { publicApiEndpoint } from '@newtg/public-shell'
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
  const url = publicApiEndpoint(serverAddress)
  if (url === '') {
    return { url: PLACEHOLDER_BASE_URL, placeholder: true }
  }
  return { url, placeholder: false }
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
    <section className="start">
      <div className="wrap">
        <div className="section-head">
          <h2 className="section-title">{t('public.home.onboarding.title')}</h2>
          <p className="note">{t('public.home.onboarding.lede')}</p>
        </div>

        <div className="start-grid">
          <ol className="steps">
            <li className="step">
              <span className="step-n" aria-hidden="true">
                01
              </span>
              <div>
                <span className="sr-only">
                  {t('public.home.onboarding.step', { number: 1 })}
                </span>
                <h3>{t('public.home.onboarding.token_title')}</h3>
                <p>{t('public.home.onboarding.token_body')}</p>
                <a href={createTokenHref} className="btn btn-sm">
                  {t('public.home.onboarding.create_token')}
                </a>
              </div>
            </li>

            <li className="step">
              <span className="step-n" aria-hidden="true">
                02
              </span>
              <div>
                <span className="sr-only">
                  {t('public.home.onboarding.step', { number: 2 })}
                </span>
                <h3>{t('public.home.onboarding.base_url_title')}</h3>
                <p>{t('public.home.onboarding.base_url_body')}</p>
                <div className="baseurl">
                  <code>{base.url}</code>
                  <button
                    type="button"
                    aria-label={t('public.home.onboarding.copy_base_url')}
                    onClick={() => void copy(base.url)}
                    className="copy-btn"
                  >
                    <Copy01Icon size={16} strokeWidth={1.5} />
                  </button>
                </div>
                {base.placeholder ? (
                  <p className="step-hint">
                    {t('public.home.onboarding.base_url_unset')}
                  </p>
                ) : null}
              </div>
            </li>

            <li className="step">
              <span className="step-n" aria-hidden="true">
                03
              </span>
              <div>
                <span className="sr-only">
                  {t('public.home.onboarding.step', { number: 3 })}
                </span>
                <h3>{t('public.home.onboarding.request_title')}</h3>
                <p>{t('public.home.onboarding.request_body')}</p>
              </div>
            </li>
          </ol>

          <div className="terminal">
            <div className="term-bar">
              <div
                role="tablist"
                aria-label={t('public.home.onboarding.lang_label')}
                className="term-tabs"
              >
                {SAMPLE_LANGS.map((item) => (
                  <button
                    key={item}
                    type="button"
                    role="tab"
                    aria-selected={lang === item}
                    onClick={() => setLang(item)}
                    className="term-tab"
                  >
                    {t(`public.home.onboarding.lang.${item}`)}
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={() => void copy(code)}
                className="term-copy"
              >
                {t('public.home.onboarding.copy_code')}
              </button>
            </div>
            <pre>
              <code className="select-text">{code}</code>
            </pre>
            <div className="term-foot">
              <span>{t('public.home.onboarding.term_base')}</span>
              <span>{t('public.home.onboarding.term_sdks')}</span>
            </div>
          </div>
        </div>
      </div>
    </section>
  )
}
