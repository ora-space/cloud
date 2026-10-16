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

/**
 * One declared Start variable, so a bounds case reads as the rule it exercises rather than its shape.
 */
function variable(name: string, extra: Partial<WorkflowInputVariable> = {}): WorkflowInputVariable {
  return { name, fieldType: 'text-input', valueType: 'string', ...extra }
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

describe('workflow run form bounds', () => {
  it('stops the author fields where the server stops projecting them', () => {
    const variables = Array.from({ length: 101 }, (_, i) => variable(`v${i}`))

    const fields = workflowRunFields(graphOf({ variables, prompt: 'go' }))

    // The hundredth variable is projected and the hundred-and-first is not, which is where the
    // server's own list ends: asking past it would be a question the run is never validated on.
    expect(fields).toHaveLength(100)
    expect(fields[0]?.key).toBe('v0')
    expect(fields[99]?.key).toBe('v99')
  })

  it('gives up the kickoff prompt rather than exceed the descriptor bound', () => {
    const variables = Array.from({ length: 100 }, (_, i) => variable(`v${i}`))

    const fields = workflowRunFields(graphOf({ variables, prompt: 'go' }))

    // The prompt is the platform's own field and the last one added, so it is the one the server
    // drops when the author's fields already fill the descriptor.
    expect(fields.map((field) => field.key)).toEqual(variables.map((entry) => entry.name))
  })

  it('shares the descriptor bound with the dotted globals', () => {
    const variables = Array.from({ length: 100 }, (_, i) => variable(`v${i}`))

    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables,
        globals: [{ name: 'config.region', valueType: 'string', value: 'eu' }],
      }),
    )

    // The globals fill whatever room the author's own questions left, rather than getting a bound
    // of their own, because the server appends them to the same list.
    expect(fields).toHaveLength(100)
    expect(fields.some((field) => field.key === 'config.region')).toBe(false)
  })

  it('drops a variable whose name is not a field key', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          variable('topic name'),
          variable('topic/name'),
          variable(' topic '),
          variable('深'),
          variable('topic'),
          variable('topic-1'),
          variable('a@b:c'),
        ],
      }),
    )

    // The server drops a name outside the grammar instead of rewriting it, because the values reach
    // the workflow keyed by name — so the form may neither offer one nor trim one into acceptance.
    expect(fields.map((field) => field.key)).toEqual(['topic', 'topic-1', 'a@b:c'])
  })

  it('drops a global whose name is not a field key', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        globals: [
          { name: 'config.region', valueType: 'string', value: 'eu' },
          { name: 'config.region zone', valueType: 'string', value: 'utc' },
        ],
      }),
    )

    expect(fields.map((field) => field.key)).toEqual(['config.region'])
  })

  it('drops a variable whose label is over the server bound, counting bytes', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          variable('asciiOver', { displayName: 'x'.repeat(201) }),
          variable('asciiAtLimit', { displayName: 'x'.repeat(200) }),
          variable('chineseOver', { displayName: '深'.repeat(67) }),
          variable('chineseAtLimit', { displayName: '深'.repeat(66) }),
        ],
      }),
    )

    // The bound counts UTF-8 bytes, so sixty-seven Chinese characters are over it where two hundred
    // ASCII ones are not.
    expect(fields.map((field) => field.key)).toEqual(['asciiAtLimit', 'chineseAtLimit'])
  })

  it('offers only the select choices the server keeps', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          variable('tone', {
            fieldType: 'select',
            options: [
              'brief',
              'brief',
              '',
              'deep',
              'x'.repeat(200),
              'y'.repeat(201),
              '深'.repeat(66),
              '深'.repeat(67),
            ],
          }),
        ],
      }),
    )

    // A repeated, empty or over-long choice is dropped by the server rather than offered, and the
    // bound counts bytes: two hundred ASCII characters are a legal choice where sixty-seven Chinese
    // ones are not. Offering one the gate does not know is a question whose only answer is a refusal.
    expect(fields[0]?.options).toEqual(['brief', 'deep', 'x'.repeat(200), '深'.repeat(66)])
  })

  it('stops the select choices where the server stops them', () => {
    const choices = Array.from({ length: 101 }, (_, index) => `c${index}`)

    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [variable('tone', { fieldType: 'select', options: choices })],
      }),
    )

    // The hundredth choice is offered and the hundred-and-first is not, which is where the server's
    // own list ends: past it the gate has no value to match an answer against.
    expect(fields[0]?.options).toHaveLength(100)
    expect(fields[0]?.options[99]).toBe('c99')
  })

  it('leaves out a default the server would refuse for its length', () => {
    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          variable('asciiOver', { fieldType: 'paragraph', value: 'x'.repeat(4001) }),
          variable('asciiAtLimit', { fieldType: 'paragraph', value: 'x'.repeat(4000) }),
          variable('chineseOver', { fieldType: 'paragraph', value: '深'.repeat(1334) }),
          variable('chineseAtLimit', { fieldType: 'paragraph', value: '深'.repeat(1333) }),
        ],
      }),
    )

    // Opening on a value the gate refuses would be worse than opening empty, and the bound counts
    // bytes: 1334 Chinese characters are 4002 of them, and 1333 are 3999.
    expect(initialWorkflowRunValues(fields)).toEqual({
      asciiAtLimit: 'x'.repeat(4000),
      chineseAtLimit: '深'.repeat(1333),
    })
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

  it('leaves out a select default that is not a choice the server kept', () => {
    const overLong = 'y'.repeat(201)

    const fields = workflowRunFields(
      graphOf({
        launchFields: NO_PROMPT,
        variables: [
          variable('tone', { fieldType: 'select', options: ['brief', overLong], value: overLong }),
        ],
      }),
    )

    // The choice is dropped from the field, so it cannot be the answer the form opens on either: the
    // gate matches a select's value against the choices it kept, and this one is not among them.
    expect(fields[0]?.options).toEqual(['brief'])
    expect(initialWorkflowRunValues(fields)).toEqual({})
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
