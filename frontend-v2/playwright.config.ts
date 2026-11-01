import { defineConfig, devices } from '@playwright/test'

// End-to-end: the real v2-server on PostgreSQL behind the real frontend, on their own ports so a
// running `task v2:dev` is untouched. Needs the PostgreSQL from `task v2:db` (or E2E_DATABASE_URL).
const database = process.env.E2E_DATABASE_URL ?? 'postgres://ora:ora-local@127.0.0.1:55432/ora_v2_e2e?sslmode=disable'
const serverPort = Number(process.env.E2E_SERVER_PORT ?? 8091)
const webPort = Number(process.env.E2E_WEB_PORT ?? 5174)
const server = `http://127.0.0.1:${serverPort}`
const web = `http://127.0.0.1:${webPort}`
const externalServers = process.env.E2E_EXTERNAL_SERVERS === '1'

export default defineConfig({
  testDir: 'e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: web,
    trace: 'retain-on-failure',
    // A local Chromium can stand in for Playwright's download, e.g. on machines without network access.
    launchOptions: process.env.PW_CHROMIUM_PATH ? { executablePath: process.env.PW_CHROMIUM_PATH } : {},
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: externalServers
    ? undefined
    : [
        {
          command: 'go run .',
          cwd: '../v2-server',
          url: `${server}/healthz`,
          env: {
            V2_ADDR: `127.0.0.1:${serverPort}`,
            V2_DATABASE_URL: database,
            // Opt-in real Agent acceptance: forward the prepared Node assets and an isolated data directory
            // so the Agent specs can run here; without them those specs skip themselves.
            ...(process.env.V2_NODE_ASSETS_DIR ? { V2_NODE_ASSETS_DIR: process.env.V2_NODE_ASSETS_DIR } : {}),
            ...(process.env.V2_DATA_DIR ? { V2_DATA_DIR: process.env.V2_DATA_DIR } : {}),
          },
          reuseExistingServer: false,
          timeout: 120_000,
        },
        {
          command: `npx vite --port ${webPort} --strictPort --host 127.0.0.1`,
          url: web,
          env: { V2_SERVER: server },
          reuseExistingServer: false,
        },
      ],
})
