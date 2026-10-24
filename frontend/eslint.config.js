import js from '@eslint/js'
import reactHooks from 'eslint-plugin-react-hooks'
import globals from 'globals'
import tseslint from 'typescript-eslint'

export default tseslint.config(
  {
    ignores: ['dist/**', 'node_modules/**', 'src/routeTree.gen.ts'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      globals: { ...globals.browser },
    },
    plugins: {
      'react-hooks': reactHooks,
    },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      'no-restricted-syntax': [
        'error',
        // A bare hex colour can enter the source through three syntax nodes;
        // the M0 rule only covered the first (string literal), so `#fff` in a
        // template string or directly in JSX silently bypassed GC-12.
        {
          selector:
            "Literal[value=/^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/]",
          message:
            'Use a CSS token from styles/tokens.css instead of a raw hex colour.',
        },
        {
          selector:
            "TemplateElement[value.raw=/^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/]",
          message:
            'Use a CSS token from styles/tokens.css instead of a raw hex colour.',
        },
        {
          // JSXText keeps the literal (and indentation) verbatim, hence the
          // optional surrounding whitespace.
          selector:
            "JSXText[value=/^\\s*#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})\\s*$/]",
          message:
            'Use a CSS token from styles/tokens.css instead of a raw hex colour.',
        },
        // CJK hardcoding enters the source through the same three node shapes
        // as a bare hex colour. Matching only `Literal` let a Chinese string in
        // a template literal or in JSX text bypass the i18n gate silently.
        {
          selector: 'Literal[value=/[\\u4e00-\\u9fff]/]',
          message:
            'Use an i18n key from src/locales instead of a CJK string literal.',
        },
        {
          selector: 'TemplateElement[value.raw=/[\\u4e00-\\u9fff]/]',
          message:
            'Use an i18n key from src/locales instead of a CJK string literal.',
        },
        {
          selector: 'JSXText[value=/[\\u4e00-\\u9fff]/]',
          message:
            'Use an i18n key from src/locales instead of a CJK string literal.',
        },
        // Type-scale drift (docs/modules-00 §4.7): px-sized text utilities and
        // the legacy Tailwind default scale are banned. The semantic tokens in
        // styles/tailwind.css (text-body/meta/badge/page-title/section/
        // card-title/stat/lead/display-*) carry size + line-height + weight.
        {
          selector: 'Literal[value=/text-\\[\\d/]',
          message:
            'Use a §4.7 type token (text-body/text-meta/text-badge/…) instead of a px-sized text-[Npx] utility.',
        },
        {
          selector: 'TemplateElement[value.raw=/text-\\[\\d/]',
          message:
            'Use a §4.7 type token (text-body/text-meta/text-badge/…) instead of a px-sized text-[Npx] utility.',
        },
        {
          selector:
            'Literal[value=/\\btext-(xs|sm|base|lg|xl|[2-9]xl)\\b/]',
          message:
            'Use a §4.7 type token (text-body/text-meta/text-badge/text-page-title/…) instead of the default text-xs–9xl scale.',
        },
        {
          selector:
            'TemplateElement[value.raw=/\\btext-(xs|sm|base|lg|xl|[2-9]xl)\\b/]',
          message:
            'Use a §4.7 type token (text-body/text-meta/text-badge/text-page-title/…) instead of the default text-xs–9xl scale.',
        },
      ],
    },
  },
  {
    files: ['*.config.ts', '*.config.js', 'eslint.config.js'],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
)
