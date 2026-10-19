import { defineConfig, devices } from '@playwright/test';

const port = 4173;

// e2e runs in a real browser over the MSW mocks (`npm run dev:mock`). It is not part of
// `npm run reviewable`, which needs no browser: see README.md for how to run it.
export default defineConfig({
  testDir: './e2e',
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      // PLAYWRIGHT_CHANNEL=chrome uses an installed Google Chrome instead of Playwright's build.
      use: {
        ...devices['Desktop Chrome'],
        channel: process.env.PLAYWRIGHT_CHANNEL,
      },
    },
  ],
  webServer: {
    command: `npm run dev:mock -- --port ${port} --strictPort --host 127.0.0.1`,
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: !process.env.CI,
  },
});
