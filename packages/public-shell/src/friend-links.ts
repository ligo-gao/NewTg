import type { ShellFooterLink } from './types'

/**
 * 友情链接 — real, stable destinations to the model providers and the
 * community this gateway fronts. Brand names are not translated; only the
 * column title comes from the shell dictionary (`footer.friends`).
 *
 * Both apps render this same list, so the footer's third column cannot drift
 * between the marketing site and the SPA. Every href is an absolute https URL
 * and therefore opens in a new tab under the shell's external-link rule.
 */
export const FRIEND_LINKS: readonly ShellFooterLink[] = [
  { label: 'OpenAI', href: 'https://openai.com', external: true },
  { label: 'Anthropic', href: 'https://www.anthropic.com', external: true },
  { label: 'Google AI', href: 'https://ai.google', external: true },
  { label: 'DeepSeek', href: 'https://www.deepseek.com', external: true },
  { label: '通义千问', href: 'https://tongyi.aliyun.com', external: true },
  { label: 'Hugging Face', href: 'https://huggingface.co', external: true },
]
