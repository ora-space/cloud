import { defineConfig } from 'orval'

// Temporary v2-only task routes; the formal Cloud contract keeps tasks under issues and runs, so
// this client retires with v2-server rather than with the formal one.
export default defineConfig({
  tasks: {
    input: { target: '../v2-server/tasks.openapi.json' },
    output: {
      target: 'src/features/collaboration/tasks-api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
})
