import { describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'

// 与后端契约测试共享的建档体：Go 侧
// backend/internal/httpapi/handlers_model_catalog_wizard_contract_test.go 读取同一
// 文件并断言后端接受它。只改一边的形状会让另一侧变红。
import createFixture from '../../../../../api/fixtures/model-profile-wizard-create.json'
import type { FetchChannelModelEntry } from './types'
import {
  applyDefaults,
  buildCreateBody,
  buildRows,
  buildUpdateBody,
  endpointsObject,
  formatParameterLines,
  knowledgeCutoffIsValid,
  parametersAreValid,
  parseParameterLines,
  rowActionOf,
  rowIssues,
  vendorStateOf,
  type ParameterRow,
  type RowIssue,
  type WizardDefaults,
  type WizardRow,
} from './model-profile-wizard-logic'

type ModelMeta = components['schemas']['ModelMeta']

/** `noUncheckedIndexedAccess` 下索引访问是 `T | undefined`；测试里固定取首行。 */
function firstRow(rows: WizardRow[]): WizardRow {
  const row = rows[0]
  if (row === undefined) throw new Error('expected at least one wizard row')
  return row
}

function wizardRow(overrides: Partial<WizardRow> = {}): WizardRow {
  return {
    id: 'gpt-4o',
    displayName: '',
    description: '',
    contextLength: '',
    maxOutput: '',
    releasedAt: '',
    endpoints: [],
    vendorName: '',
    capabilities: [],
    modalities: [],
    parameters: [],
    knowledgeCutoff: '',
    tags: [],
    factsSource: '',
    selected: true,
    overwrite: false,
    ...overrides,
  }
}

function modelMeta(overrides: Partial<ModelMeta> = {}): ModelMeta {
  return {
    id: 1,
    model_name: 'gpt-4o',
    display_name: null,
    description: null,
    icon: null,
    tags: [],
    vendor_id: null,
    endpoints: {},
    capabilities: [],
    modalities: [],
    parameters: [],
    context_length: null,
    max_output: null,
    knowledge_cutoff: null,
    released_at: null,
    status: 1,
    sync_official: true,
    name_rule: 0,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-20T08:00:00Z',
    vendor_name: null,
    square_state: 'hidden',
    configured_channel_count: 0,
    enable_groups: [],
    ...overrides,
  }
}

function entry(overrides: Partial<FetchChannelModelEntry> = {}): FetchChannelModelEntry {
  return {
    id: 'gpt-4o',
    display_name: '',
    description: '',
    context_length: null,
    max_output: null,
    released_at: '',
    endpoints: [],
    vendor_suggestion: '',
    facts_source: '',
    ...overrides,
  }
}

function emptyDefaults(): WizardDefaults {
  return {
    vendorName: '',
    endpoints: [],
    capabilities: [],
    modalities: [],
    parameters: [],
    knowledgeCutoff: '',
    tags: [],
  }
}

describe('endpointsObject', () => {
  it('maps public enum values to model_meta path objects', () => {
    expect(endpointsObject(['chat.completions', 'generateContent'])).toEqual({
      '/v1/chat/completions': ['POST'],
      '/v1beta/models/{model}:generateContent': ['POST'],
    })
  })
})

describe('buildRows', () => {
  it('selects models without a profile plus the preselected existing ones', () => {
    const existing = new Map<string, ModelMeta>([
      ['gpt-4o', modelMeta({ id: 1, model_name: 'gpt-4o', vendor_name: 'OpenAI' })],
      ['gpt-4o-mini', modelMeta({ id: 2, model_name: 'gpt-4o-mini' })],
    ])
    const rows = buildRows(
      [
        entry({ id: 'gpt-4o', vendor_suggestion: 'suggested-vendor' }),
        entry({ id: 'gpt-4o-mini' }),
        entry({ id: 'o3' }),
      ],
      existing,
      ['gpt-4o-mini'],
    )

    expect(rows.map((row) => [row.id, row.selected, row.vendorName, row.overwrite])).toEqual([
      ['gpt-4o', false, 'OpenAI', false],
      ['gpt-4o-mini', true, '', false],
      ['o3', true, '', false],
    ])
  })

  it('never infers capabilities or modalities from the model name', () => {
    const row = firstRow(
      buildRows(
        [
          entry({
            id: 'gpt-4o',
            display_name: 'GPT-4o',
            context_length: 128000,
            endpoints: ['chat.completions'],
          }),
        ],
        new Map(),
        [],
      ),
    )

    expect(row).toMatchObject({
      displayName: 'GPT-4o',
      contextLength: '128000',
      maxOutput: '',
      endpoints: ['chat.completions'],
      capabilities: [],
      modalities: [],
      parameters: [],
      knowledgeCutoff: '',
      tags: [],
    })
  })

  it('seeds the editable collections from the existing profile (deep copies)', () => {
    const profile = modelMeta({
      capabilities: ['text', 'vision'],
      modalities: ['image'],
      parameters: [
        { name: 'stream', type: 'boolean', required: true, description: 'toggle' },
      ],
      knowledge_cutoff: '2025-05',
      tags: ['legacy'],
    })
    const row = firstRow(
      buildRows(
        [entry({ id: 'gpt-4o' })],
        new Map([['gpt-4o', profile]]),
        ['gpt-4o'],
      ),
    )

    expect(row).toMatchObject({
      capabilities: ['text', 'vision'],
      modalities: ['image'],
      knowledgeCutoff: '2025-05',
      tags: ['legacy'],
    })
    expect(row.parameters).toEqual([
      {
        name: 'stream',
        type: 'boolean',
        description: 'toggle',
        defaultValue: '',
        required: true,
      },
    ])

    // 行编辑不能反向改到查询缓存里的档案。
    row.capabilities.push('reasoning')
    row.tags.push('added')
    const parameter = row.parameters[0]
    if (parameter !== undefined) parameter.name = 'changed'
    expect(profile.capabilities).toEqual(['text', 'vision'])
    expect(profile.tags).toEqual(['legacy'])
    expect(profile.parameters[0]?.name).toBe('stream')
  })
})

describe('applyDefaults', () => {
  const rows = [wizardRow({ capabilities: ['text'], knowledgeCutoff: '' })]
  const defaults = {
    ...emptyDefaults(),
    capabilities: ['vision'],
    knowledgeCutoff: '2025-06',
  }

  it('keeps existing values when emptyOnly is on', () => {
    expect(firstRow(applyDefaults(rows, defaults, true)).capabilities).toEqual(['text'])
    expect(firstRow(applyDefaults(rows, defaults, true)).knowledgeCutoff).toBe('2025-06')
  })

  it('replaces values when emptyOnly is off', () => {
    expect(firstRow(applyDefaults(rows, defaults, false)).capabilities).toEqual(['vision'])
  })

  it('never overwrites a filled vendor with an empty default', () => {
    const filled = [wizardRow({ vendorName: 'Anthropic' })]
    expect(firstRow(applyDefaults(filled, emptyDefaults(), false)).vendorName).toBe('Anthropic')
    expect(
      firstRow(applyDefaults(filled, { ...emptyDefaults(), vendorName: 'OpenAI' }, true)).vendorName,
    ).toBe('Anthropic')
  })
})

describe('buildCreateBody', () => {
  it('matches the shared fixture the backend contract test also reads', () => {
    const row = wizardRow({
      id: 'gemini-2.5-pro',
      displayName: 'Gemini 2.5 Pro',
      description: 'Frontier multimodal model',
      tags: ['vision', 'reasoning'],
      endpoints: ['chat.completions', 'generateContent'],
      capabilities: ['text', 'vision', 'function_calling'],
      modalities: ['text', 'image'],
      parameters: [
        {
          name: 'temperature',
          type: 'number',
          description: 'Sampling temperature',
          defaultValue: '0.7',
          required: false,
        },
        {
          name: 'stream',
          type: 'boolean',
          description: '',
          defaultValue: 'false',
          required: false,
        },
      ],
      contextLength: '1048576',
      maxOutput: '65536',
      knowledgeCutoff: '2025-06',
      releasedAt: '2025-06-17',
    })

    expect(buildCreateBody(row, 7)).toEqual(createFixture)
  })

  it('only sends the fields the operator filled in', () => {
    const body = buildCreateBody(
      wizardRow({ id: 'o3', endpoints: ['messages'], capabilities: ['text'] }),
      null,
    )

    expect(body).toEqual({
      model_name: 'o3',
      vendor_id: null,
      endpoints: { '/v1/messages': ['POST'] },
      capabilities: ['text'],
    })
  })

  it('writes the numeric and date fields it can parse', () => {
    const body = buildCreateBody(
      wizardRow({
        contextLength: '64000',
        maxOutput: '8192',
        knowledgeCutoff: '2025-06',
        releasedAt: '2025-04-16',
      }),
      3,
    )

    expect(body.context_length).toBe(64000)
    expect(body.max_output).toBe(8192)
    expect(body.knowledge_cutoff).toBe('2025-06')
    expect(body.released_at).toBe('2025-04-16')
    expect(body.vendor_id).toBe(3)
  })

  it('drops parameter rows whose type is outside the contract enum', () => {
    const body = buildCreateBody(
      wizardRow({
        parameters: [
          { name: 'temperature', type: 'decimal', description: '', defaultValue: '' },
          { name: 'stream', type: 'boolean', description: '', defaultValue: '' },
        ],
      }),
      null,
    )

    expect(body.parameters).toEqual([{ name: 'stream', type: 'boolean', required: false }])
  })
})

describe('buildUpdateBody', () => {
  it('preserves fields the wizard does not own and never wipes existing values with blanks', () => {
    const body = buildUpdateBody(
      wizardRow({ displayName: '' }),
      modelMeta({ icon: 'bot', status: 2, display_name: 'Existing Name' }),
      7,
    )

    expect(body.icon).toBe('bot')
    expect(body.status).toBe(2)
    expect(body.display_name).toBe('Existing Name')
    expect(body.vendor_id).toBe(7)
    expect(body.updated_at).toBe('2026-10-20T08:00:00Z')
  })

  it('falls back to the existing arrays when the row is blank', () => {
    const existing = modelMeta({
      endpoints: { '/v1/messages': ['POST'] },
      capabilities: ['vision'],
      modalities: ['image'],
      parameters: [{ name: 'stream', type: 'boolean' }],
      tags: ['legacy'],
      vendor_id: 4,
    })
    const body = buildUpdateBody(wizardRow(), existing, null)

    expect(body.endpoints).toEqual({ '/v1/messages': ['POST'] })
    expect(body.capabilities).toEqual(['vision'])
    expect(body.modalities).toEqual(['image'])
    expect(body.parameters).toEqual([{ name: 'stream', type: 'boolean' }])
    expect(body.tags).toEqual(['legacy'])
    expect(body.vendor_id).toBe(4)
    expect(body.name_rule).toBe(0)
    expect(body.sync_official).toBe(true)
  })

  it('writes non-empty row values instead of the existing ones', () => {
    const existing = modelMeta({
      display_name: 'Old Name',
      description: 'old description',
      tags: ['old-tag'],
      endpoints: { '/v1/messages': ['POST'] },
      knowledge_cutoff: '2024-01',
    })
    const body = buildUpdateBody(
      wizardRow({
        displayName: 'New Name',
        description: 'new description',
        tags: ['new-tag'],
        endpoints: ['chat.completions'],
        knowledgeCutoff: '2025-06',
      }),
      existing,
      7,
    )

    expect(body.display_name).toBe('New Name')
    expect(body.description).toBe('new description')
    expect(body.tags).toEqual(['new-tag'])
    expect(body.endpoints).toEqual({ '/v1/chat/completions': ['POST'] })
    expect(body.knowledge_cutoff).toBe('2025-06')
  })

  it('falls back to existing numbers when the row value is not a non-negative integer', () => {
    const existing = modelMeta({ context_length: 128000, max_output: 8192 })
    const body = buildUpdateBody(
      wizardRow({ contextLength: 'abc', maxOutput: '-1' }),
      existing,
      null,
    )

    expect(body.context_length).toBe(128000)
    expect(body.max_output).toBe(8192)
  })

  it('writes a valid row number over the existing one', () => {
    const existing = modelMeta({ context_length: 128000 })
    const body = buildUpdateBody(
      wizardRow({ contextLength: '200000' }),
      existing,
      null,
    )

    expect(body.context_length).toBe(200000)
  })

  it('keeps numeric and boolean parameter defaults when overwriting', () => {
    const existing = modelMeta({
      parameters: [
        { name: 'temperature', type: 'number', default: 0 },
        { name: 'stream', type: 'boolean', default: false },
      ],
    })
    const rows = buildRows(
      [entry({ id: 'gpt-4o' })],
      new Map([['gpt-4o', existing]]),
      ['gpt-4o'],
    )

    expect(firstRow(rows).parameters).toEqual([
      {
        name: 'temperature',
        type: 'number',
        description: '',
        defaultValue: '0',
      },
      {
        name: 'stream',
        type: 'boolean',
        description: '',
        defaultValue: 'false',
      },
    ])
    // 覆盖后写入体仍带 "0" / "false"，不会被静默丢成空串。
    expect(buildUpdateBody(firstRow(rows), existing, null).parameters).toEqual([
      { name: 'temperature', type: 'number', required: false, default: '0' },
      { name: 'stream', type: 'boolean', required: false, default: 'false' },
    ])
  })

  it('trims the description before writing it', () => {
    const created = buildCreateBody(wizardRow({ description: '  padded  ' }), null)
    const updated = buildUpdateBody(wizardRow({ description: '  padded  ' }), modelMeta(), null)

    expect(created.description).toBe('padded')
    expect(updated.description).toBe('padded')
  })

  it('preserves the required flag carried by an existing parameter', () => {
    const body = buildUpdateBody(
      wizardRow({
        parameters: [
          {
            name: 'stream',
            type: 'boolean',
            description: '',
            defaultValue: '',
            required: true,
          },
        ],
      }),
      modelMeta(),
      null,
    )

    expect(body.parameters).toEqual([
      { name: 'stream', type: 'boolean', required: true },
    ])
  })
})

describe('parseParameterLines', () => {
  it('parses name:type lines and drops malformed ones', () => {
    expect(parseParameterLines('temperature:number\n\nbadline\ntop_p:string:distribution')).toEqual([
      { name: 'temperature', type: 'number', description: '', defaultValue: '' },
      { name: 'top_p', type: 'string', description: 'distribution', defaultValue: '' },
    ])
  })

  it('drops types outside the contract enum', () => {
    expect(parseParameterLines('temperature:decimal\nstream:boolean')).toEqual([
      { name: 'stream', type: 'boolean', description: '', defaultValue: '' },
    ])
  })
})

describe('formatParameterLines', () => {
  it('round-trips the name:type[:description] text form', () => {
    const rows: ParameterRow[] = [
      { name: 'temperature', type: 'number', description: '', defaultValue: '' },
      { name: 'top_p', type: 'string', description: 'distribution', defaultValue: '' },
    ]

    expect(formatParameterLines(rows)).toBe('temperature:number\ntop_p:string:distribution')
    expect(parseParameterLines(formatParameterLines(rows))).toEqual(rows)
  })
})

describe('rowIssues', () => {
  const blankParameter: ParameterRow = {
    name: '',
    type: '',
    description: '',
    defaultValue: '',
  }

  const cases: Array<{ name: string; row?: WizardRow; expected: RowIssue[] }> = [
    { name: 'accepts a blank row', expected: [] },
    {
      name: 'allows exactly 64 runes in the display name',
      // U+540D 是双字节 CJK 字符；用码点构造避免源码里出现中文字面量。
      row: wizardRow({ displayName: String.fromCodePoint(0x540d).repeat(64) }),
      expected: [],
    },
    {
      name: 'rejects 65 runes in the display name',
      row: wizardRow({ displayName: String.fromCodePoint(0x540d).repeat(65) }),
      expected: ['displayName'],
    },
    {
      name: 'allows exactly 512 runes in the description',
      row: wizardRow({ description: 'x'.repeat(512) }),
      expected: [],
    },
    {
      name: 'rejects 513 runes in the description',
      row: wizardRow({ description: 'x'.repeat(513) }),
      expected: ['description'],
    },
    {
      name: 'accepts blank numbers and zero',
      row: wizardRow({ contextLength: '', maxOutput: '0' }),
      expected: [],
    },
    {
      name: 'rejects non-numeric and negative numbers',
      row: wizardRow({ contextLength: 'abc', maxOutput: '-1' }),
      expected: ['contextLength', 'maxOutput'],
    },
    {
      name: 'rejects fractional numbers',
      row: wizardRow({ contextLength: '1.5' }),
      expected: ['contextLength'],
    },
    {
      name: 'accepts a YYYY-MM knowledge cutoff',
      row: wizardRow({ knowledgeCutoff: '2025-06' }),
      expected: [],
    },
    {
      name: 'rejects other knowledge cutoff shapes',
      row: wizardRow({ knowledgeCutoff: '2025-13' }),
      expected: ['knowledgeCutoff'],
    },
    {
      name: 'accepts untouched parameter rows',
      row: wizardRow({ parameters: [blankParameter] }),
      expected: [],
    },
    {
      name: 'rejects parameter types outside the contract enum',
      row: wizardRow({
        parameters: [
          { name: 'temperature', type: 'decimal', description: '', defaultValue: '' },
        ],
      }),
      expected: ['parameters'],
    },
    {
      name: 'rejects a parameter without a type',
      row: wizardRow({
        parameters: [{ name: 'temperature', type: '', description: '', defaultValue: '' }],
      }),
      expected: ['parameters'],
    },
    {
      name: 'rejects a parameter without a name',
      row: wizardRow({
        parameters: [{ name: '', type: 'number', description: '', defaultValue: '' }],
      }),
      expected: ['parameters'],
    },
  ]

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(rowIssues(testCase.row ?? wizardRow())).toEqual(testCase.expected)
    })
  }
})

describe('knowledgeCutoffIsValid', () => {
  const cases: Array<[string, boolean]> = [
    ['', true],
    ['2025-06', true],
    ['1999-12', true],
    [' 2025-06 ', true],
    ['2025-6', false],
    ['2025-13', false],
    ['2025-00', false],
    ['2025-06-01', false],
  ]

  for (const [value, expected] of cases) {
    it(`${JSON.stringify(value)} → ${String(expected)}`, () => {
      expect(knowledgeCutoffIsValid(value)).toBe(expected)
    })
  }
})

describe('parametersAreValid', () => {
  const cases: Array<{ name: string; rows: ParameterRow[]; expected: boolean }> = [
    { name: 'accepts an empty list', rows: [], expected: true },
    {
      name: 'accepts fully blank rows',
      rows: [{ name: ' ', type: ' ', description: '', defaultValue: '' }],
      expected: true,
    },
    {
      name: 'accepts a complete supported row',
      rows: [{ name: 'stream', type: 'boolean', description: '', defaultValue: '' }],
      expected: true,
    },
    {
      name: 'rejects an unsupported type',
      rows: [{ name: 'stream', type: 'decimal', description: '', defaultValue: '' }],
      expected: false,
    },
    {
      name: 'rejects a name without a type',
      rows: [{ name: 'stream', type: '', description: '', defaultValue: '' }],
      expected: false,
    },
    {
      name: 'rejects a type without a name',
      rows: [{ name: '', type: 'boolean', description: '', defaultValue: '' }],
      expected: false,
    },
  ]

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(parametersAreValid(testCase.rows)).toBe(testCase.expected)
    })
  }
})

describe('rowActionOf', () => {
  it('creates when there is no profile', () => {
    expect(rowActionOf(wizardRow(), undefined)).toBe('create')
  })

  it('skips an unlocked profile unless the row opts in', () => {
    expect(rowActionOf(wizardRow(), modelMeta())).toBe('skip')
    expect(rowActionOf(wizardRow({ overwrite: true }), modelMeta())).toBe('update')
  })

  it('always skips a locally locked profile, even with overwrite checked', () => {
    const locked = modelMeta({ sync_official: false })
    expect(rowActionOf(wizardRow(), locked)).toBe('skip')
    expect(rowActionOf(wizardRow({ overwrite: true }), locked)).toBe('skip')
  })
})

describe('vendorStateOf', () => {
  const directory = {
    ids: new Map([['openai', 3]]),
    nameById: new Map([[3, 'OpenAI']]),
  }

  it('treats a blank name as unset', () => {
    expect(vendorStateOf('', directory, '')).toEqual({ kind: 'unset' })
    expect(vendorStateOf('   ', directory, '')).toEqual({ kind: 'unset' })
  })

  it('resolves a directory hit case-insensitively to the canonical name', () => {
    expect(vendorStateOf(' OPENAI ', directory, '')).toEqual({
      kind: 'listed',
      name: 'OpenAI',
      id: 3,
    })
  })

  it('marks the explicit step-2 new vendor name as to-be-created', () => {
    expect(vendorStateOf('Acme', directory, ' acme ')).toEqual({
      kind: 'new',
      name: 'Acme',
    })
  })

  it('leaves other suggestions unresolved instead of creating them', () => {
    expect(vendorStateOf('Gemini', directory, 'Acme')).toEqual({
      kind: 'unresolved',
      name: 'Gemini',
    })
    expect(vendorStateOf('Gemini', directory, '')).toEqual({
      kind: 'unresolved',
      name: 'Gemini',
    })
  })
})
