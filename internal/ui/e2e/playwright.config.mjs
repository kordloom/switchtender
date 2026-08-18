import { defineConfig, devices } from "@playwright/test";

// The port the smoke tests drive. Fixed rather than random so the webServer url and the baseURL agree
// without threading a value between them.
const PORT = 18777;
const BASE = `http://127.0.0.1:${PORT}`;

// The web server is a real switchtender demo instance: it seeds sample projects, templates,
// inventories, and runs by executing a handful of real playbooks, so the pages under test render the
// same shapes an operator sees rather than fixtures. Seeding takes a moment, so the readiness timeout is
// generous. reuseExistingServer lets a developer point the tests at a demo they already have running.
export default defineConfig({
  testDir: ".",
  testMatch: "*.spec.mjs",
  // A real browser start plus a seeded server is not a unit test, so the per-test budget is wider than
  // Playwright's default while still bounding a hang.
  timeout: 60_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: BASE,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    // ST_E2E_CHROMIUM, when set, points at a Chromium binary to drive instead of the one Playwright
    // manages. It exists for a machine that already has the full browser but not the headless shell
    // Playwright downloads by default; CI installs both and leaves this unset.
    launchOptions: process.env.ST_E2E_CHROMIUM
      ? { executablePath: process.env.ST_E2E_CHROMIUM }
      : {},
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        // ST_E2E_CHANNEL, when set (e.g. "chrome"), drives a system-installed browser instead of the
        // one Playwright downloads. It is a convenience for a machine that already has Chrome; CI
        // leaves it unset and uses the pinned bundled Chromium so the browser version is reproducible.
        ...(process.env.ST_E2E_CHANNEL ? { channel: process.env.ST_E2E_CHANNEL } : {}),
      },
    },
  ],
  webServer: {
    command:
      process.env.ST_E2E_SERVE_CMD ||
      `./.bin/switchtender demo --addr 127.0.0.1:${PORT}`,
    url: `${BASE}/healthz`,
    timeout: 120_000,
    reuseExistingServer: !process.env.CI,
    stdout: "pipe",
    stderr: "pipe",
  },
});
