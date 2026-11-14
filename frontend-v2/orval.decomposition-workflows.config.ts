import { defineConfig } from 'orval'

// Temporary picker-only workflow verdicts; the general workflow authoring client stays independent
// of task decomposition and this client retires with v2-server.
export default defineConfig({
  decompositionWorkflows: {
    input: { target: '../v2-server/decomposition-workflows.openapi.json' },
    output: {
      target: 'src/features/collaboration/decomposition-api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
})
