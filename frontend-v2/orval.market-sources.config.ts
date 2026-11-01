import { defineConfig } from 'orval'

// Temporary v2-only routes; the formal Cloud contract remains unchanged.
export default defineConfig({
  marketSources: {
    input: { target: '../v2-server/market-sources.openapi.json' },
    output: {
      target: 'src/features/settings/market-api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
})
