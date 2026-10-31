import { defineConfig } from 'orval'

// Temporary v2-only chat routes (v2-server/chat.openapi.json); the formal Cloud contract has no
// standalone conversations yet. Projects come from v2-server's candidate project routes, since it
// does not serve the formal v1 ones.
export default defineConfig({
  chat: {
    input: { target: '../v2-server/chat.openapi.json' },
    output: {
      target: 'src/features/chat/api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
  projects: {
    input: { target: '../v2-server/internal/projectrepos/projects.openapi.json' },
    output: {
      target: 'src/lib/projects-api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
})
