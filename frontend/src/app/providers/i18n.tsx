import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useQuery } from '@tanstack/react-query'

import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { publicStatusQueryOptions } from '@/lib/public-status'
import {
  selfSettingsQueryOptions,
  SELF_SETTINGS_QUERY_KEY,
} from '@/lib/self-settings'
import {
  I18nContextProvider,
  type I18nContextValue,
  type Locale,
  type TranslateValues,
} from '@/shared/lib/i18n-context'

// The context/hook live in shared/lib so shared components (e.g. the toast
// viewport) can consume them without importing app/ (web-00 §2.2). Re-exported
// here to keep the existing `app/providers/i18n` import path stable.
export { useI18n } from '@/shared/lib/i18n-context'
export type {
  I18nContextValue,
  Locale,
  TranslateFn,
  TranslateValues,
} from '@/shared/lib/i18n-context'

/**
 * i18n 运行时（web-00 §7）。
 *
 * 词典位于 `locales/<lang>/<module>.json`；文件内键是 `{feature}.{element}`，
 * 运行时补回模块命名空间，因此对外键始终是 `{module}.{feature}.{element}`。
 *
 * 加载按域进行（web-00 §7.2）：进入某域路由时动态 `import()` 该域词典，
 * 首屏（公网页 JS ≤150KB，§3.4）不会打包控制台词典。`common` 是唯一常驻
 * 基线（全局 Toast/错误文案）；其余域仅在调用方请求时加载。下面的 loader
 * 映射同时声明了「随包发布的语言」与「可用域」，打包器会为每一项生成
 * 独立异步 chunk。
 *
 * 语言集合的权威来自 `GET /api/status`（§7.3，不写死）；本文件的 7 语
 * 只是「随包提供翻译」的子集，运行时会把服务端下发的集合与它求交。
 */

/** 动态 `import()` 后的 JSON 模块形状。 */
type JsonDictionary = { default: Record<string, unknown> }

/**
 * 域名称是 `{module}.{feature}.{element}` 的 `<module>` 段。
 *
 * 这个联合类型刻意显式声明而不是从 JSON 模块推断：词典增长后
 * `typeof import(...)` 的字面量类型会大到触发 TS7056（序列化上限），
 * 一个显式注解既消除了该问题，也让「可用域」保持稳定契约。
 */
export type DictionaryModule =
  | 'common'
  | 'public'
  | 'auth'
  | 'account'
  | 'admin'
  | 'logs'
  | 'settings'

/**
 * `locale → domain → loader`。每一项都必须是懒加载：这里若写成静态 import，
 * 词典会被并回入口 chunk，直接破坏 §7.2 的首屏预算。
 */
const DICTIONARY_LOADERS: Record<
  Locale,
  Record<DictionaryModule, () => Promise<JsonDictionary>>
> = {
  en: {
    common: () => import('@/locales/en/common.json'),
    public: () => import('@/locales/en/public.json'),
    auth: () => import('@/locales/en/auth.json'),
    account: () => import('@/locales/en/account.json'),
    admin: () => import('@/locales/en/admin.json'),
    logs: () => import('@/locales/en/logs.json'),
    // M6c FE-08 keeps the settings domain separate from `admin` so the
    // settings shell only downloads its own dictionary (web-00 §7.2).
    settings: () => import('@/locales/en/settings.json'),
  },
  'zh-CN': {
    common: () => import('@/locales/zh-CN/common.json'),
    public: () => import('@/locales/zh-CN/public.json'),
    auth: () => import('@/locales/zh-CN/auth.json'),
    account: () => import('@/locales/zh-CN/account.json'),
    admin: () => import('@/locales/zh-CN/admin.json'),
    logs: () => import('@/locales/zh-CN/logs.json'),
    settings: () => import('@/locales/zh-CN/settings.json'),
  },
  'zh-TW': {
    common: () => import('@/locales/zh-TW/common.json'),
    public: () => import('@/locales/zh-TW/public.json'),
    auth: () => import('@/locales/zh-TW/auth.json'),
    account: () => import('@/locales/zh-TW/account.json'),
    admin: () => import('@/locales/zh-TW/admin.json'),
    logs: () => import('@/locales/zh-TW/logs.json'),
    settings: () => import('@/locales/zh-TW/settings.json'),
  },
  fr: {
    common: () => import('@/locales/fr/common.json'),
    public: () => import('@/locales/fr/public.json'),
    auth: () => import('@/locales/fr/auth.json'),
    account: () => import('@/locales/fr/account.json'),
    admin: () => import('@/locales/fr/admin.json'),
    logs: () => import('@/locales/fr/logs.json'),
    settings: () => import('@/locales/fr/settings.json'),
  },
  ru: {
    common: () => import('@/locales/ru/common.json'),
    public: () => import('@/locales/ru/public.json'),
    auth: () => import('@/locales/ru/auth.json'),
    account: () => import('@/locales/ru/account.json'),
    admin: () => import('@/locales/ru/admin.json'),
    logs: () => import('@/locales/ru/logs.json'),
    settings: () => import('@/locales/ru/settings.json'),
  },
  ja: {
    common: () => import('@/locales/ja/common.json'),
    public: () => import('@/locales/ja/public.json'),
    auth: () => import('@/locales/ja/auth.json'),
    account: () => import('@/locales/ja/account.json'),
    admin: () => import('@/locales/ja/admin.json'),
    logs: () => import('@/locales/ja/logs.json'),
    settings: () => import('@/locales/ja/settings.json'),
  },
  vi: {
    common: () => import('@/locales/vi/common.json'),
    public: () => import('@/locales/vi/public.json'),
    auth: () => import('@/locales/vi/auth.json'),
    account: () => import('@/locales/vi/account.json'),
    admin: () => import('@/locales/vi/admin.json'),
    logs: () => import('@/locales/vi/logs.json'),
    settings: () => import('@/locales/vi/settings.json'),
  },
}

/** 随包发布的 7 语（canonical，默认 `en`）。 */
export const BUNDLED_LOCALES = Object.keys(DICTIONARY_LOADERS) as Locale[]
export const DEFAULT_LOCALE: Locale = 'en'
/** 兼容旧引用：M0 的 SUPPORTED_LOCALES 语义即「随包语言」。 */
export const SUPPORTED_LOCALES = BUNDLED_LOCALES

const LOCALE_STORAGE_KEY = 'newtg.language'
const MISSING_KEY_WARNING = import.meta.env.DEV

function isLocale(value: string): value is Locale {
  return (BUNDLED_LOCALES as readonly string[]).includes(value)
}

/** 读取本地记忆。隐私模式/禁用存储时静默失败（§7.3 也允许无 localStorage）。 */
function readStoredLocale(): Locale | null {
  try {
    const value = globalThis.localStorage?.getItem(LOCALE_STORAGE_KEY)
    return value !== undefined && value !== null && isLocale(value) ? value : null
  } catch {
    return null
  }
}

function writeStoredLocale(locale: Locale): void {
  try {
    globalThis.localStorage?.setItem(LOCALE_STORAGE_KEY, locale)
  } catch {
    // 本地记忆不可用不影响当前会话：语言已在内存状态中生效。
  }
}

/**
 * 把浏览器标签（`navigator.language`）归一为 canonical 7 语（§7.3）。
 *
 * `zh-Hant`/`zh-TW`/`zh-HK`/`zh-MO` → `zh-TW`；其余 `zh-*`（含裸 `zh`）
 * → `zh-CN`。裸 `zh` 无脚本/地区信息，按使用面最广的简体处理；这是归一
 * 规则而非新契约（契约仍是 canonical 标签）。不在集合内返回 null。
 */
export function matchLocale(tag: string | null | undefined): Locale | null {
  if (tag === null || tag === undefined) return null
  const lower = tag.toLowerCase()
  if (lower === 'zh' || lower.startsWith('zh-')) {
    if (/(hant|tw|hk|mo)/.test(lower)) return 'zh-TW'
    return 'zh-CN'
  }
  const base = lower.split('-')[0] ?? ''
  return isLocale(base) ? (base as Locale) : null
}

/**
 * 初始语言决定顺序的前半段（§7.3）：本地记忆 → `navigator.language` → `en`。
 * 服务端偏好优先级最高，但它必须异步读取，由 `LocalePreferenceSync` 在
 * 挂载后覆盖；先同步给出一个可渲染的语言，避免首屏闪烁成键名。
 */
export function resolveInitialLocale(): Locale {
  const stored = readStoredLocale()
  if (stored !== null) return stored
  const navigatorTag =
    typeof navigator === 'undefined' ? undefined : navigator.language
  return matchLocale(navigatorTag) ?? DEFAULT_LOCALE
}

/** 当前激活语言。路由 loader 在 React 之外执行，需要这条模块级镜像。 */
let activeLocale: Locale = resolveInitialLocale()

export function getActiveLocale(): Locale {
  return activeLocale
}

function setActiveLocale(locale: Locale): void {
  activeLocale = locale
  writeStoredLocale(locale)
}

/**
 * 服务端下发的语言集合与随包词典求交，并保持服务端顺序（dropdown 顺序即
 * status 顺序，F-01-19）。若 status 缺失或交集为空（后端尚未升级），退回
 * 随包 7 语，保证语言卡仍可用。
 */
export function resolveAvailableLocales(
  advertised: readonly string[] | undefined,
): readonly Locale[] {
  if (advertised === undefined) return BUNDLED_LOCALES
  const matched = advertised.filter(isLocale)
  return matched.length > 0 ? matched : BUNDLED_LOCALES
}

function flatten(
  prefix: string,
  messages: Record<string, unknown>,
  out: Record<string, string>,
): void {
  for (const [key, value] of Object.entries(messages)) {
    const fullKey = `${prefix}.${key}`
    if (typeof value === 'string') {
      out[fullKey] = value
    } else if (typeof value === 'object' && value !== null) {
      flatten(fullKey, value as Record<string, unknown>, out)
    }
  }
}

type FlatDictionary = Record<string, string>

/**
 * 展开后的词典缓存，只由 `loadOne` 填充。按「语言 × 域」分开缓存，才能让
 * `translate` 区分「尚未 import」与「键确实缺失」，避免预加载前每次渲染
 * 都告警。
 */
const dictionaries: Partial<
  Record<Locale, Partial<Record<DictionaryModule, FlatDictionary>>>
> = {}

/** 进行中的 import，并发调用共享同一个 chunk 请求。 */
const pendingLoads = new Map<string, Promise<FlatDictionary>>()

/** 路由 loader 请求过的域；语言切换时 provider 据此补齐当前语言的词典。 */
const requestedModules = new Set<DictionaryModule>()

function isDictionaryLoaded(
  locale: Locale,
  module: DictionaryModule,
): boolean {
  return dictionaries[locale]?.[module] !== undefined
}

async function loadOne(
  locale: Locale,
  module: DictionaryModule,
): Promise<FlatDictionary> {
  const cached = dictionaries[locale]?.[module]
  if (cached !== undefined) return cached

  const cacheKey = `${locale}:${module}`
  const inflight = pendingLoads.get(cacheKey)
  if (inflight !== undefined) return inflight

  const promise = DICTIONARY_LOADERS[locale][module]()
    .then((json) => {
      const flat: FlatDictionary = {}
      flatten(module, json.default, flat)
      const localeDictionaries = (dictionaries[locale] ??= {})
      localeDictionaries[module] = flat
      return flat
    })
    .finally(() => {
      // 失败的 import 不能永久污染缓存：下一个调用方（如重新挂载）会重试。
      pendingLoads.delete(cacheKey)
    })

  pendingLoads.set(cacheKey, promise)
  return promise
}

/**
 * 加载、展开并缓存一个域词典（§7.2）。
 *
 * 非默认语言会同时加载 `en` 同域词典：§7.2 的回退链是「当前语言 → en →
 * 键名」，若 en 未加载，一次漏译就会直接渲染键名。两者都是异步 chunk，
 * 不进入首屏路径。
 */
export async function loadDictionary(
  locale: Locale,
  module: DictionaryModule,
): Promise<FlatDictionary> {
  requestedModules.add(module)
  if (locale === DEFAULT_LOCALE) return loadOne(locale, module)
  const [, dict] = await Promise.all([
    loadOne(DEFAULT_LOCALE, module),
    loadOne(locale, module),
  ])
  return dict
}

/**
 * 路由 loader 专用入口：按「当前激活语言」加载该域（语言镜像由 provider
 * 在语言切换时同步更新），并保证 en 回退词典同时就绪。
 */
export async function loadRouteDictionary(
  module: DictionaryModule,
): Promise<void> {
  await loadDictionary(getActiveLocale(), module)
}

function interpolate(template: string, values?: TranslateValues): string {
  if (values === undefined) return template
  return template.replace(/\{([a-zA-Z0-9_]+)\}/g, (match, name: string) => {
    const value = values[name]
    return value === undefined ? match : String(value)
  })
}

function moduleOf(key: string): DictionaryModule {
  // 键是 `{module}.{feature}.{element}`；没有点就无法命名域，整串返回并在
  // `translate` 的「已知模块」检查中失败，走缺键告警。
  const separator = key.indexOf('.')
  return (separator === -1 ? key : key.slice(0, separator)) as DictionaryModule
}

/** 供 provider 调用的纯查询；导出以便测试与非 React 调用方使用。 */
export function translate(
  locale: Locale,
  key: string,
  values?: TranslateValues,
): string {
  const module = moduleOf(key)
  const dictionary = dictionaries[locale]?.[module]

  // §7.2 回退链：当前语言 → en → 键名本身。
  let template = dictionary?.[key]
  if (template === undefined && locale !== DEFAULT_LOCALE) {
    template = dictionaries[DEFAULT_LOCALE]?.[module]?.[key]
  }

  if (template === undefined) {
    const moduleIsKnown = Object.prototype.hasOwnProperty.call(
      DICTIONARY_LOADERS[locale],
      module,
    )
    // 已知域尚未 import 属于「待加载」，不是缺键：provider 会在 chunk 到达后
    // 触发重渲染。只有已加载域缺少该键（或域拼写错误）才是真正的缺失，
    // 仅在 dev 告警（§7.2）。
    if (MISSING_KEY_WARNING && (dictionary !== undefined || !moduleIsKnown)) {
      console.warn(`[i18n] missing key: ${key} (${locale})`)
    }
    // 缺键时渲染键名而不是空白（§7.2）。
    return key
  }
  return interpolate(template, values)
}

/**
 * 应用级文案（Toast、错误）都在 `common`，因此它是常驻基线。控制台域保持
 * 按需加载，绝不能进入公网首屏路径（web-00 §7.2、§3.4）。
 */
const DEFAULT_MODULES: readonly DictionaryModule[] = ['common']

export interface I18nProviderProps {
  children: ReactNode
  locale?: Locale
  /**
   * 本子树要预取的域。`common` 是常驻基线；需要其它词典的路由代码传入
   * 自己的稳定列表，或调用 `loadDictionary`，这样公网页不会下载它从不渲染
   * 的控制台域（web-00 §7.2）。
   */
  modules?: readonly DictionaryModule[]
  /**
   * 是否挂载偏好同步器（服务端语言 + status 语言集合）。只有 `AppProviders`
   * 打开它——该组件依赖 React Query 的 Provider，而测试里单独渲染
   * `I18nProvider` 时不应触发任何网络请求。
   */
  syncPreference?: boolean
}

export function I18nProvider({
  children,
  locale: initialLocale,
  modules = DEFAULT_MODULES,
  syncPreference = false,
}: I18nProviderProps) {
  const [locale, setLocaleState] = useState<Locale>(
    () => initialLocale ?? resolveInitialLocale(),
  )
  const [availableLocales, setAvailableLocalesState] = useState<
    readonly Locale[]
  >(BUNDLED_LOCALES)
  // 词典是模块级可变状态，provider 需要显式信号在懒加载 chunk 到达后重渲染。
  const [readyRevision, setReadyRevision] = useState(0)

  // 当前语言的同步镜像：回调里需要读到最新值，而不能依赖闭包里的旧 state。
  const localeRef = useRef(locale)
  const availableLocalesRef = useRef<readonly Locale[]>(BUNDLED_LOCALES)
  /**
   * 用户是否做过显式切换。服务端偏好是异步到达的：若用户在响应返回前已经
   * 主动选了语言，初始偏好不得把它覆盖回去（评审 F1）。只在用户手势后置位，
   * 服务端/状态归一化都不置位。
   */
  const userGestureRef = useRef(false)

  const commitLocale = useCallback((next: Locale) => {
    localeRef.current = next
    setActiveLocale(next)
    setLocaleState(next)
  }, [])

  /** 用户手势入口（context.setLocale）；置位后服务端偏好不再回写。 */
  const setLocale = useCallback(
    (next: Locale) => {
      userGestureRef.current = true
      commitLocale(next)
    },
    [commitLocale],
  )

  /**
   * 状态归一化（F5）：激活语言必须落在 status 下发的集合内；不在集合内时
   * 优先回退 en，其次集合首项。这不是用户手势，因此不动 gesture 标记。
   */
  const applyAvailableLocales = useCallback(
    (next: readonly Locale[]) => {
      availableLocalesRef.current = next
      setAvailableLocalesState(next)
      if (next.length === 0) return
      if (!next.includes(localeRef.current)) {
        commitLocale(
          next.includes(DEFAULT_LOCALE) ? DEFAULT_LOCALE : (next[0] ?? DEFAULT_LOCALE),
        )
      }
    },
    [commitLocale],
  )

  /** 服务端偏好入口：仅在用户尚未做显式切换时生效（§7.3 优先级 + F1）。 */
  const applyServerLocale = useCallback(
    (next: Locale) => {
      if (userGestureRef.current) return
      const available = availableLocalesRef.current
      if (!available.includes(next)) {
        applyAvailableLocales(available)
        return
      }
      commitLocale(next)
    },
    [applyAvailableLocales, commitLocale],
  )

  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  // TC-C-02：语言切换必须同步更新 <html lang> 与 document.title。站点名在
  // common 域；chunk 未到时沿用键名检查，避免把原始键写进标题栏。
  useEffect(() => {
    const title = translate(locale, 'common.app.title')
    if (title !== 'common.app.title') document.title = title
  }, [locale, readyRevision])

  // modules 可能是内联数组；用内容作为依赖，避免每次渲染都重跑加载 effect。
  const moduleKey = modules.join(',')
  useEffect(() => {
    let cancelled = false
    // 路由 loader 曾请求过的域也要在此次语言切换后补齐；否则用户在控制台
    // 切语言时，当前页面的域词典仍是旧语言（回退成英文）。
    const wanted = new Set<DictionaryModule>([...modules, ...requestedModules])
    const pendingModules = [...wanted].filter(
      (module) => !isDictionaryLoaded(locale, module),
    )
    if (pendingModules.length === 0) return

    void Promise.all(
      pendingModules.map((module) => loadDictionary(locale, module)),
    )
      .then(() => {
        if (!cancelled) setReadyRevision((revision) => revision + 1)
      })
      .catch((error: unknown) => {
        // 失败的 chunk 不能让应用白屏：`translate` 继续返回键名（可见、可调试），
        // 下次挂载会重试。
        console.error('[i18n] failed to load dictionary', error)
      })

    return () => {
      cancelled = true
    }
    // moduleKey 代表 modules 的内容；依赖数组保持稳定。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [locale, moduleKey])

  const value = useMemo<I18nContextValue>(
    () => ({
      locale,
      availableLocales,
      setLocale,
      t: (key, values) => {
        // 读取 revision，让闭包在懒加载完成后重建；`translate` 本身只读
        // 模块级缓存。
        void readyRevision
        return translate(locale, key, values)
      },
    }),
    [locale, readyRevision, availableLocales, setLocale],
  )

  return (
    <I18nContextProvider value={value}>
      {children}
      {syncPreference ? (
        <LocalePreferenceSync
          onServerLocale={applyServerLocale}
          onAvailableLocales={applyAvailableLocales}
        />
      ) : null}
    </I18nContextProvider>
  )
}

interface LocalePreferenceSyncProps {
  onServerLocale: (locale: Locale) => void
  onAvailableLocales: (locales: readonly Locale[]) => void
}

/**
 * 语言偏好同步（web-00 §7.3 的硬顺序）：
 *   1. 已登录用户的服务端偏好（GET /api/self）；
 *   2. 本地 localStorage（初始已在 `resolveInitialLocale` 中生效）；
 *   3. navigator.language；
 *   4. en。
 *
 * 服务端偏好只能异步读取，因此挂载后一旦拿到就覆盖当前语言；但用户已经
 * 主动切换过时不再覆盖（`onServerLocale` 内部判断手势）。
 *
 * 为什么用 `enabled: false` 的 me 查询：`/api/me` 已由控制台路由守卫写入
 * 同一 QueryClient 的缓存。关闭 fetch 的 observer 仍会订阅缓存更新，
 * 因此只有「确实已登录」时才会发 `/api/self`，公网匿名访问不会多打一次
 * 401 请求。
 */
function LocalePreferenceSync({
  onServerLocale,
  onAvailableLocales,
}: LocalePreferenceSyncProps) {
  // status 由公网页/守卫共享；这里给 5 分钟 staleTime，避免 i18n 观察者
  // 挂载时对同一份 no-store 响应再发一次请求（评审 F9）。语言清单变化
  // 频率远低于站点开关，下一次导航/刷新仍会重新校验。
  const status = useQuery({
    ...publicStatusQueryOptions(),
    staleTime: 5 * 60_000,
  })
  const me = useQuery({ ...meQueryOptions(), enabled: false, queryKey: ME_QUERY_KEY })
  const self = useQuery({
    ...selfSettingsQueryOptions(),
    enabled: me.isSuccess,
    queryKey: SELF_SETTINGS_QUERY_KEY,
  })

  useEffect(() => {
    if (status.data !== undefined) {
      onAvailableLocales(resolveAvailableLocales(status.data.languages))
    }
  }, [status.data, onAvailableLocales])

  useEffect(() => {
    if (self.data !== undefined) {
      onServerLocale(self.data.language)
    }
  }, [self.data, onServerLocale])

  return null
}
