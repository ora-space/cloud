import type { ModelConnection } from '@/features/model-connections/api'
import type { AgentConfiguration } from './api'

/** Synthetic owned connection used by this module's protocol-boundary tests. */
export function agentConnection(overrides: Partial<ModelConnection> = {}): ModelConnection {
  return {
    id: 'mc1',
    name: 'Personal Messages',
    protocol: 'anthropic-messages',
    baseUrl: 'https://models.example.invalid',
    authMode: 'bearer',
    enabled: true,
    credentialConfigured: true,
    version: 1,
    models: [{ id: 'vendor/model', name: 'Private Model', contextWindow: 128000, maxTokens: 8192 }],
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

/**
 * OpenCode context: the plugin stores no connection, so its model comes from the
 * user's shared global default and credentials follow that default.
 */
export function opencodeContext(overrides: Partial<AgentConfiguration> = {}): AgentConfiguration {
  return agentContext({
    pluginId: 'official/ora-space.opencode',
    connectionId: null,
    modelId: null,
    modelDefault: { connectionId: 'mc1', modelId: 'vendor/model', version: 2 },
    ...overrides,
  })
}

/** A fresh compiled declaration/context; callers may emulate another revision without shared state. */
export function agentContext(overrides: Partial<AgentConfiguration> = {}): AgentConfiguration {
  return {
    pluginId: 'official/ora-space.claude',
    spaceId: 'space1',
    pluginVersion: '1.0.0',
    revision: 1,
    selectionVersion: 3,
    declarationFingerprint: 'a'.repeat(64),
    cleared: false,
    requiresRepair: false,
    settings: { style: 'concise' },
    connectionId: 'mc1',
    modelId: 'vendor/model',
    declaration: {
      schemaVersion: 2,
      fingerprint: 'a'.repeat(64),
      modelAccess: {
        contractVersion: 1,
        modelCardinality: 'single',
        protocols: ['anthropic-messages'],
      },
      settings: [
        {
          id: 'style',
          title: '风格',
          description: '输出风格',
          settingType: 'string',
          required: true,
          order: 1,
          default: null,
        },
        {
          id: 'budget',
          title: '预算',
          description: '可选数值',
          settingType: 'number',
          required: false,
          order: 2,
          default: 5,
        },
        {
          id: 'verbose',
          title: '详细输出',
          description: '可选开关',
          settingType: 'boolean',
          required: false,
          order: null,
          default: false,
        },
      ],
    },
    ...overrides,
  }
}
