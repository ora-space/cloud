import { defineConfig } from 'orval'

// Temporary v2-only workflow definition and run routes (v2-server/workflows.openapi.json). The client
// lives under src/lib because both the workflows page and the chat page's workflow runs call it.
export default defineConfig({
  workflows: {
    input: { target: '../v2-server/workflows.openapi.json' },
    output: {
      target: 'src/lib/workflows/api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: {
        mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' },
        query: { version: 5 },
      },
    },
  },
})
