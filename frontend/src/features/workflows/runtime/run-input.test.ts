import { describe, expect, it } from 'vitest'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import type { WorkflowGraphEnvelope } from '@/features/workflows/runtime/graph-codec'
import {
  initialWorkflowRunValues,
  missingWorkflowRunFields,
  workflowRunFields,
  workflowRunInput,
} from '@/features/workflows/runtime/run-input'
import type {
  WorkflowGlobalVariable,
  WorkflowInputVariable,
  WorkflowLaunchField,
} from '@/features/workflows/runtime/types'

/** The prompt declaration that takes the kickoff prompt out of the form. */
const NO_PROMPT: WorkflowLaunchField[] = [{ key: 'prompt', enabled: false }]

/**
 * The envelope a published snapshot of a Start node would parse into.
 *
 * Built through the real codec rather than as a literal, so the tests read the same shape the run
 * dialog does — the snapshot's `graph` as the API returns it.
 */
function graphOf(options: {
  variables?: WorkflowInputVariable[]
  globals?: WorkflowGlobalVariable[]
  launchFields?: WorkflowLaunchField[]
  prompt?: string
}): WorkflowGraphEnvelope {
  return parseWorkflowGraphValue({
    nodes: [
      {
        id: 'start',
        type: 'workflow',
        position: { x: 0, y: 0 },
        data: {
          kind: 'start',
          title: 'Start',
          description: '',
          ...(options.prompt === undefined ? {} : { input: options.prompt }),
          ...(options.variables === undefined ? {} : { inputVariables: options.variables }),
        },
      },
    ],
    edges: [],
    viewport: { x: 0, y: 0, zoom: 1 },
    globalVariables: options.globals ?? [],
    launchFields: options.launchFields ?? [],
  })
}

describe('workflow run form projection', () => {
  it('asks the version kickoff prompt first, prefilled from the Start node', () => {
    const fields = workflowRunFields(graphOf({ prompt: '  Summarize the release notes  ' }))

    expect(fields).toEqual([
      {
        key: 'prompt',
        label: 'prompt',
        required: false,
        control: 'textarea',
        options: [],
        defaultValue: 'Summarize the release notes',
      },
    ])
  })

  it('drops the kickoff prompt when the launch declaration turned it off', () => {
    expect(workflowRunFields(graphOf({ prompt: 'ignored', launchFields: NO_PROMPT }))).toEqual([])
  })

  it('opens the prompt empty rather than on blank space', () => {
    const fields = workflowRunFields(graphOf({ prompt: '   ' }))

    expect(fields).toEqual([
      { key: 'prompt', label: 'prompt', required: false, control: 'textarea', options: [] },
    ])
  })

  it('insists on the prompt only when the declaration does', () => {
    const fields = workflowRunFields(graphOf({ launchFields: [{ key: 'prompt', required: true }] }))

    expect(fields.map((field) => field.required)).toEqual([true])
  })

  it('projects the declared variables in order, with their own controls', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          { name: 'topic', displayName: '主题', fieldType: 'text-input', valueType: 'string' },
          { name: 'tone', fieldType: 'select', valueType: 'string', options: ['brief', 'deep'] },
          { name: 'count', fieldType: 'number', valueType: 'number', required: true },
        ],
      }),
    )

    expect(fields).toEqual([
      {
        key: 'topic',
        label: '主题',
        required: false,
        control: 'text',
        options: [],
      },
      {
        key: 'tone',
        label: 'tone',
        required: false,
        control: 'select',
        options: ['brief', 'deep'],
      },
      {
        key: 'count',
        label: 'count',
        required: true,
        control: 'number',
        options: [],
      },
    ])
  })

  it('degrades a select with nothing to choose from to a text field', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [{ name: 'tone', fieldType: 'select', valueType: 'string', options: [] }],
      }),
    )

    expect(fields).toEqual([
      { key: 'tone', label: 'tone', required: false, control: 'text', options: [] },
    ])
  })

  it('collects a file reference and a document as the text that carries them', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          { name: 'source', fieldType: 'file', valueType: 'file' },
          { name: 'manifest', fieldType: 'json', valueType: 'object' },
        ],
      }),
    )

    expect(fields.map((field) => field.control)).toEqual(['text', 'textarea'])
  })

  it('leaves the prompt key to an author who declared it themselves', () => {
    const fields = workflowRunFields(
      graphOf({
        prompt: 'the initial prompt',
        variables: [
          { name: 'prompt', displayName: '提示词', fieldType: 'paragraph', valueType: 'string' },
        ],
      }),
    )

    expect(fields).toEqual([
      {
        key: 'prompt',
        label: '提示词',
        required: false,
        control: 'textarea',
        options: [],
        defaultValue: 'the initial prompt',
      },
    ])
  })

  it('keeps the value the author declared for the prompt key over the initial prompt', () => {
    const fields = workflowRunFields(
      graphOf({
        prompt: 'the initial prompt',
        variables: [
          { name: 'prompt', fieldType: 'text-input', valueType: 'string', value: 'theirs' },
        ],
      }),
    )

    expect(fields.map((field) => field.defaultValue)).toEqual(['theirs'])
  })

  it('skips a variable the form cannot name', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          { name: '   ', fieldType: 'text-input', valueType: 'string' },
          { name: 'kept', fieldType: 'text-input', valueType: 'string' },
        ],
      }),
    )

    expect(fields.map((field) => field.key)).toEqual(['kept'])
  })

  it('adds the dotted globals after the declared variables and skips the plain ones', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [{ name: 'topic', fieldType: 'text-input', valueType: 'string' }],
        globals: [
          { name: 'config.region', valueType: 'string', value: 'eu' },
          { name: 'plainConstant', valueType: 'string', value: 'unused' },
          { name: 'config.retries', valueType: 'integer' },
        ],
      }),
    )

    expect(fields).toEqual([
      { key: 'topic', label: 'topic', required: false, control: 'text', options: [] },
      {
        key: 'config.region',
        label: 'config.region',
        required: false,
        control: 'text',
        options: [],
        defaultValue: 'eu',
      },
      {
        key: 'config.retries',
        label: 'config.retries',
        required: false,
        control: 'number',
        options: [],
      },
    ])
  })

  it('lets a declared variable keep a name a global also uses', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [{ name: 'config.region', fieldType: 'text-input', valueType: 'string' }],
        globals: [{ name: 'config.region', valueType: 'string', value: 'global' }],
      }),
    )

    expect(fields).toEqual([
      {
        key: 'config.region',
        label: 'config.region',
        required: false,
        control: 'text',
        options: [],
      },
    ])
  })
})

describe('workflow run form defaults', () => {
  it('seeds only the defaults the form can render', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          { name: 'topic', fieldType: 'text-input', valueType: 'string', value: 'release' },
          { name: 'count', fieldType: 'number', valueType: 'number', value: 'many' },
          {
            name: 'tone',
            fieldType: 'select',
            valueType: 'string',
            options: ['brief'],
            value: 'deep',
          },
        ],
      }),
    )

    expect(initialWorkflowRunValues(fields)).toEqual({ topic: 'release' })
  })

  it('keeps a select default that is one of its choices', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          {
            name: 'tone',
            fieldType: 'select',
            valueType: 'string',
            options: ['brief'],
            value: 'brief',
          },
        ],
      }),
    )

    expect(initialWorkflowRunValues(fields)).toEqual({ tone: 'brief' })
  })
})

describe('workflow run form gating', () => {
  const fields = workflowRunFields(
    graphOf({
      launchFields: NO_PROMPT,
      variables: [
        { name: 'topic', fieldType: 'text-input', valueType: 'string', required: true },
        { name: 'note', fieldType: 'paragraph', valueType: 'string' },
      ],
    }),
  )

  it('reports a required field the member left unanswered', () => {
    expect(missingWorkflowRunFields(fields, {}).map((field) => field.key)).toEqual(['topic'])
  })

  it('counts blank space as unanswered', () => {
    expect(missingWorkflowRunFields(fields, { topic: '   ' }).map((field) => field.key)).toEqual([
      'topic',
    ])
  })

  it('leaves an optional field out of the gate', () => {
    expect(missingWorkflowRunFields(fields, { topic: 'release' })).toEqual([])
  })

  it('omits the unanswered fields from the submitted input', () => {
    expect(workflowRunInput(fields, { topic: 'release', note: '' })).toEqual({ topic: 'release' })
  })

  it('sends an answer of false and zero rather than treating it as absent', () => {
    const booleans = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          { name: 'dryRun', fieldType: 'checkbox', valueType: 'boolean' },
          { name: 'count', fieldType: 'number', valueType: 'number' },
        ],
      }),
    )

    expect(workflowRunInput(booleans, { dryRun: false, count: 0 })).toEqual({
      dryRun: false,
      count: 0,
    })
  })
})
