/**
 * 全球模型性能测评的数据结构与示例数据源。
 *
 * 【数据来源诚实声明】排行榜后端目前没有测评接口，这里是一份**演示用
 * 静态样例**：它让「全球性能测评」面板与详情弹窗按设计稿完整呈现。面板
 * 的 meta 与弹窗底部都必须显示示例数据标记（`rankings.benchmark.sample`、
 * `sample_note`）；接入 `/api/benchmarks/*` 后整份数据应被替换，而不是
 * 继续留在生产页面上。
 *
 * 所有面向界面的文案只存 i18n key，不存任何语言的字符串：分类标题、指标
 * 标签、模型信息字段、分位描述都走 `public.rankings.benchmark.*`。基准名
 * （MMLU、HumanEval、LMSYS Arena Elo）是国际通用专名，作为数据原样展示。
 */

export interface BenchmarkScore {
  /** 基准专名（MMLU、HumanEval…），国际通用写法，不翻译。 */
  name: string
  score: number
  /** 全球分位：前 N%。与 `rank` 二选一，都没有时弹窗不渲染这一行。 */
  topPercent?: number
  /** 全球名次。与 `topPercent` 二选一。 */
  rank?: number
  /** 分位后的补充说明（学科数、语言、难度等级），见 `benchmark.tag.*`。 */
  tagKey?: string
  highlight?: boolean
}

export interface BenchmarkCategory {
  /** i18n key，见 `public.rankings.benchmark.category.*`。 */
  titleKey: string
  benchmarks: BenchmarkScore[]
}

export interface BenchmarkDetail {
  /** i18n key，见 `public.rankings.benchmark.detail.*`。 */
  labelKey: string
  value?: string
  /** 值本身需要翻译时（例如「未公开」）给出 key，优先于 `value`。 */
  valueKey?: string
}

export interface ModelBenchmark {
  modelId: string
  name: string
  provider: string
  categories: BenchmarkCategory[]
  details: BenchmarkDetail[]
}

const CATEGORY = {
  general: 'public.rankings.benchmark.category.general',
  code: 'public.rankings.benchmark.category.code',
  math: 'public.rankings.benchmark.category.math',
  reasoning: 'public.rankings.benchmark.category.reasoning',
  safety: 'public.rankings.benchmark.category.safety',
} as const

const DETAIL = {
  paramSize: 'public.rankings.benchmark.detail.param_size',
  context: 'public.rankings.benchmark.detail.context',
  cutoff: 'public.rankings.benchmark.detail.cutoff',
  released: 'public.rankings.benchmark.detail.released',
  tested: 'public.rankings.benchmark.detail.tested',
  tester: 'public.rankings.benchmark.detail.tester',
} as const

const TAG = {
  subjects: 'public.rankings.benchmark.tag.subjects',
  python: 'public.rankings.benchmark.tag.python',
  competition: 'public.rankings.benchmark.tag.competition',
  chinese: 'public.rankings.benchmark.tag.chinese',
} as const

const UNDISCLOSED = 'public.rankings.benchmark.undisclosed'

export const BENCHMARK_DATABASE: Record<string, ModelBenchmark> = {
  'claude-opus-5': {
    modelId: 'claude-opus-5',
    name: 'Claude Opus 5',
    provider: 'Anthropic',
    categories: [
      {
        titleKey: CATEGORY.general,
        benchmarks: [
          { name: 'MMLU', score: 89.2, topPercent: 1, tagKey: TAG.subjects, highlight: true },
          { name: 'LMSYS Arena Elo', score: 1284, rank: 1, highlight: true },
          { name: 'AGIEval', score: 87.3, topPercent: 2 },
        ],
      },
      {
        titleKey: CATEGORY.code,
        benchmarks: [
          { name: 'HumanEval', score: 92.0, topPercent: 1, tagKey: TAG.python, highlight: true },
          { name: 'MBPP', score: 88.7, topPercent: 2 },
          { name: 'LiveCodeBench', score: 45.8, topPercent: 3 },
        ],
      },
      {
        titleKey: CATEGORY.math,
        benchmarks: [
          { name: 'GSM8K', score: 94.7, topPercent: 1 },
          { name: 'MATH', score: 78.5, topPercent: 2, tagKey: TAG.competition },
          { name: 'MathVista', score: 72.3, topPercent: 3 },
        ],
      },
      {
        titleKey: CATEGORY.reasoning,
        benchmarks: [
          { name: 'HellaSwag', score: 95.3, topPercent: 1 },
          { name: 'WinoGrande', score: 91.2, topPercent: 2 },
          { name: 'ARC-Challenge', score: 96.8, topPercent: 1 },
        ],
      },
      {
        titleKey: CATEGORY.safety,
        benchmarks: [
          { name: 'TruthfulQA', score: 78.4, topPercent: 5 },
          { name: 'BBH', score: 86.9, topPercent: 3 },
        ],
      },
    ],
    details: [
      { labelKey: DETAIL.paramSize, valueKey: UNDISCLOSED },
      { labelKey: DETAIL.context, value: '200K tokens' },
      { labelKey: DETAIL.cutoff, value: '2026-04' },
      { labelKey: DETAIL.released, value: '2026-09' },
      { labelKey: DETAIL.tested, value: '2026-10-20' },
      { labelKey: DETAIL.tester, value: 'LMSYS / OpenAI' },
    ],
  },
  'gpt-4.5-turbo': {
    modelId: 'gpt-4.5-turbo',
    name: 'GPT-4.5 Turbo',
    provider: 'OpenAI',
    categories: [
      {
        titleKey: CATEGORY.general,
        benchmarks: [
          { name: 'MMLU', score: 88.7, topPercent: 2, highlight: true },
          { name: 'LMSYS Arena Elo', score: 1276, rank: 2, highlight: true },
          { name: 'AGIEval', score: 85.9, topPercent: 3 },
        ],
      },
      {
        titleKey: CATEGORY.code,
        benchmarks: [
          { name: 'HumanEval', score: 90.2, topPercent: 3, highlight: true },
          { name: 'MBPP', score: 86.4, topPercent: 4 },
          { name: 'LiveCodeBench', score: 43.2, topPercent: 5 },
        ],
      },
      {
        titleKey: CATEGORY.math,
        benchmarks: [
          { name: 'GSM8K', score: 93.2, topPercent: 2 },
          { name: 'MATH', score: 74.8, topPercent: 4 },
          { name: 'MathVista', score: 69.7, topPercent: 5 },
        ],
      },
      {
        titleKey: CATEGORY.reasoning,
        benchmarks: [
          { name: 'HellaSwag', score: 94.1, topPercent: 3 },
          { name: 'WinoGrande', score: 89.5, topPercent: 4 },
          { name: 'ARC-Challenge', score: 95.3, topPercent: 2 },
        ],
      },
      {
        titleKey: CATEGORY.safety,
        benchmarks: [
          { name: 'TruthfulQA', score: 76.8, topPercent: 8 },
          { name: 'BBH', score: 84.2, topPercent: 5 },
        ],
      },
    ],
    details: [
      { labelKey: DETAIL.paramSize, valueKey: UNDISCLOSED },
      { labelKey: DETAIL.context, value: '128K tokens' },
      { labelKey: DETAIL.cutoff, value: '2026-03' },
      { labelKey: DETAIL.released, value: '2026-08' },
      { labelKey: DETAIL.tested, value: '2026-10-18' },
      { labelKey: DETAIL.tester, value: 'OpenAI / LMSYS' },
    ],
  },
  'qwen-2.5-max': {
    modelId: 'qwen-2.5-max',
    name: 'Qwen 2.5 Max',
    provider: 'Alibaba Cloud',
    categories: [
      {
        titleKey: CATEGORY.general,
        benchmarks: [
          { name: 'MMLU', score: 84.8, topPercent: 6, highlight: true },
          { name: 'C-Eval', score: 91.6, topPercent: 1, tagKey: TAG.chinese, highlight: true },
          { name: 'CMMLU', score: 90.2, topPercent: 2, tagKey: TAG.chinese },
        ],
      },
      {
        titleKey: CATEGORY.code,
        benchmarks: [
          { name: 'HumanEval', score: 86.5, topPercent: 8, highlight: true },
          { name: 'MBPP', score: 82.3, topPercent: 10 },
          { name: 'HumanEval-CN', score: 88.9, topPercent: 3, tagKey: TAG.chinese },
        ],
      },
      {
        titleKey: CATEGORY.math,
        benchmarks: [
          { name: 'GSM8K', score: 89.4, topPercent: 10 },
          { name: 'MATH', score: 68.7, topPercent: 12 },
          { name: 'CMATH', score: 85.3, topPercent: 2, tagKey: TAG.chinese },
        ],
      },
      {
        titleKey: CATEGORY.reasoning,
        benchmarks: [
          { name: 'HellaSwag', score: 91.8, topPercent: 8 },
          { name: 'WinoGrande', score: 86.4, topPercent: 10 },
          { name: 'CLUE', score: 89.7, topPercent: 3, tagKey: TAG.chinese },
        ],
      },
      {
        titleKey: CATEGORY.safety,
        benchmarks: [
          { name: 'TruthfulQA', score: 72.8, topPercent: 15 },
          { name: 'BBH', score: 79.5, topPercent: 12 },
        ],
      },
    ],
    details: [
      { labelKey: DETAIL.paramSize, valueKey: UNDISCLOSED },
      { labelKey: DETAIL.context, value: '128K tokens' },
      { labelKey: DETAIL.cutoff, value: '2026-06' },
      { labelKey: DETAIL.released, value: '2026-09' },
      { labelKey: DETAIL.tested, value: '2026-10-20' },
      { labelKey: DETAIL.tester, value: 'FudanNLP / Tsinghua' },
    ],
  },
  'gpt-4o': {
    modelId: 'gpt-4o',
    name: 'GPT-4o',
    provider: 'OpenAI',
    categories: [
      {
        titleKey: CATEGORY.general,
        benchmarks: [
          { name: 'MMLU', score: 83.7, topPercent: 8, highlight: true },
          { name: 'LMSYS Arena Elo', score: 1229, rank: 6, highlight: true },
          { name: 'AGIEval', score: 82.4, topPercent: 8 },
        ],
      },
      {
        titleKey: CATEGORY.code,
        benchmarks: [
          { name: 'HumanEval', score: 87.2, topPercent: 6, highlight: true },
          { name: 'MBPP', score: 83.8, topPercent: 8 },
          { name: 'LiveCodeBench', score: 40.5, topPercent: 10 },
        ],
      },
      {
        titleKey: CATEGORY.math,
        benchmarks: [
          { name: 'GSM8K', score: 90.1, topPercent: 8 },
          { name: 'MATH', score: 70.2, topPercent: 10 },
          { name: 'MathVista', score: 65.8, topPercent: 12 },
        ],
      },
      {
        titleKey: CATEGORY.reasoning,
        benchmarks: [
          { name: 'HellaSwag', score: 92.4, topPercent: 9 },
          { name: 'WinoGrande', score: 87.3, topPercent: 8 },
          { name: 'ARC-Challenge', score: 93.6, topPercent: 6 },
        ],
      },
      {
        titleKey: CATEGORY.safety,
        benchmarks: [
          { name: 'TruthfulQA', score: 74.6, topPercent: 12 },
          { name: 'BBH', score: 81.7, topPercent: 9 },
        ],
      },
    ],
    details: [
      { labelKey: DETAIL.paramSize, valueKey: UNDISCLOSED },
      { labelKey: DETAIL.context, value: '128K tokens' },
      { labelKey: DETAIL.cutoff, value: '2023-10' },
      { labelKey: DETAIL.released, value: '2024-05' },
      { labelKey: DETAIL.tested, value: '2026-10-25' },
      { labelKey: DETAIL.tester, value: 'OpenAI / LMSYS' },
    ],
  },
}

export interface GlobalBenchmarkEntry {
  rank: number
  modelId: string
  displayName: string
  provider: string
  eloScore: number
  mmlu: number
  humaneval: number
  /**
   * 榜单第三块徽章的基准名。默认 C-Eval 仅用于中文模型（demo 对
   * Qwen / DeepSeek 展示 C-Eval），其余模型展示 LMSYS Arena。
   */
  thirdBenchmark: 'arena' | 'ceval'
}

/** 示例数据的标注日期，面板 meta 与弹窗共用，避免两处各写一个日期。 */
export const GLOBAL_BENCHMARK_UPDATED_AT = '2026-10-25'

/**
 * 全球性能测评榜单（示例数据）。
 * 生产环境应从后端 API 获取：`/api/benchmarks/leaderboard`。
 */
export const GLOBAL_BENCHMARK_LEADERBOARD: GlobalBenchmarkEntry[] = [
  {
    rank: 1,
    modelId: 'claude-opus-5',
    displayName: 'Claude Opus 5',
    provider: 'Anthropic',
    eloScore: 1284,
    mmlu: 89.2,
    humaneval: 92.0,
    thirdBenchmark: 'arena',
  },
  {
    rank: 2,
    modelId: 'gpt-4.5-turbo',
    displayName: 'GPT-4.5 Turbo',
    provider: 'OpenAI',
    eloScore: 1276,
    mmlu: 88.7,
    humaneval: 90.2,
    thirdBenchmark: 'arena',
  },
  {
    rank: 3,
    modelId: 'gemini-ultra-1.5',
    displayName: 'Gemini Ultra 1.5',
    provider: 'Google',
    eloScore: 1262,
    mmlu: 86.4,
    humaneval: 88.4,
    thirdBenchmark: 'arena',
  },
  {
    rank: 4,
    modelId: 'claude-sonnet-3.5',
    displayName: 'Claude Sonnet 3.5',
    provider: 'Anthropic',
    eloScore: 1251,
    mmlu: 85.2,
    humaneval: 89.0,
    thirdBenchmark: 'arena',
  },
  {
    rank: 5,
    modelId: 'qwen-2.5-max',
    displayName: 'Qwen 2.5 Max',
    provider: 'Alibaba Cloud',
    eloScore: 1238,
    mmlu: 84.8,
    humaneval: 86.5,
    thirdBenchmark: 'ceval',
  },
  {
    rank: 6,
    modelId: 'gpt-4o',
    displayName: 'GPT-4o',
    provider: 'OpenAI',
    eloScore: 1229,
    mmlu: 83.7,
    humaneval: 87.2,
    thirdBenchmark: 'arena',
  },
  {
    rank: 7,
    modelId: 'llama-3.1-405b',
    displayName: 'Llama 3.1 405B',
    provider: 'Meta',
    eloScore: 1218,
    mmlu: 82.9,
    humaneval: 84.7,
    thirdBenchmark: 'arena',
  },
  {
    rank: 8,
    modelId: 'deepseek-v3',
    displayName: 'DeepSeek V3',
    provider: 'DeepSeek',
    eloScore: 1205,
    mmlu: 81.6,
    humaneval: 82.4,
    thirdBenchmark: 'ceval',
  },
]

const LEADERBOARD_BY_ID = new Map(
  GLOBAL_BENCHMARK_LEADERBOARD.map((entry) => [entry.modelId, entry]),
)

/** 第三块徽章的 C-Eval 读数：demo 只给中文模型展示，其余模型没有该值。 */
const CEVAL_BY_MODEL: Record<string, number> = {
  'qwen-2.5-max': 91.6,
  'deepseek-v3': 89.2,
}

/**
 * 榜单行的第三块徽章。中文模型展示 C-Eval（没有读数时退回 MMLU，绝不
 * 编造一个分数），其余模型展示 LMSYS Arena Elo。
 */
export function thirdBenchmark(entry: GlobalBenchmarkEntry): {
  name: string
  value: number
} {
  if (entry.thirdBenchmark === 'ceval') {
    return { name: 'C-Eval', value: CEVAL_BY_MODEL[entry.modelId] ?? entry.mmlu }
  }
  return { name: 'Arena', value: entry.eloScore }
}

/**
 * 取某个模型的完整测评数据。
 *
 * 榜单必须保证每一行都能打开详情：完整档案只覆盖前四名，其余名次用榜单
 * 自身的三项读数合成一个最小档案，而不是让点击静默失效。
 */
export function getBenchmarkData(modelId: string): ModelBenchmark | null {
  const known = BENCHMARK_DATABASE[modelId]
  if (known !== undefined) return known
  const entry = LEADERBOARD_BY_ID.get(modelId)
  if (entry === undefined) return null
  const ceval = CEVAL_BY_MODEL[modelId]
  return {
    modelId: entry.modelId,
    name: entry.displayName,
    provider: entry.provider,
    categories: [
      {
        titleKey: CATEGORY.general,
        benchmarks: [
          { name: 'MMLU', score: entry.mmlu, highlight: true },
          { name: 'HumanEval', score: entry.humaneval, highlight: true },
          { name: 'LMSYS Arena Elo', score: entry.eloScore, highlight: true },
          ...(ceval === undefined
            ? []
            : [{ name: 'C-Eval', score: ceval, tagKey: TAG.chinese }]),
        ],
      },
    ],
    details: [],
  }
}
