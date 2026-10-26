import { describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'

import type { FetchChannelModelEntry } from './types'
import {
  applyDefaults,
  buildCreateBody,
  buildRows,
  buildUpdateBody,
  endpointsObject,
  formatParameterLines,
  parseParameterLines,
  type ParameterRow,
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
