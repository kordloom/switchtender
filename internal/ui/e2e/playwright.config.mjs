import { defineConfig, devices } from "@playwright/test";

// Three servers back five suites. The smoke suite drives the seeded, read-only demo, where the data
// is rich and nothing can be changed, so it covers rendering and navigation of every main page. The
// interactive suite drives a writable serve instance, where it exercises the mutating flows the demo
// disables: launching a run and creating objects, then checking the change actually landed. The
// assess suite drives the browser assessment, a static page, from the site directory itself. The
// workflow suite drives the workflow editor against both the demo and the serve instance, and the
// workflow-integration suite runs what it builds against the serve instance.
const DEMO_PORT = 18777;
const SERVE_PORT = 18778;
const SITE_PORT = 18779;
const DEMO = `http://127.0.0.1:${DEMO_PORT}`;
const SERVE = `http://127.0.0.1:${SERVE_PORT}`;
const SITE = `http://127.0.0.1:${SITE_PORT}`;

// ST_E2E_CHANNEL, when set (e.g. "chrome"), drives a system-installed browser instead of the one
// Playwright downloads. It is a convenience for a machine that already has Chrome but not the headless
// shell Playwright fetches by default; CI installs the bundled browser and leaves this unset, so the
// browser version stays reproducible there.
const channel = process.env.ST_E2E_CHANNEL ? { channel: process.env.ST_E2E_CHANNEL } : {};

// A run submitted through the launch dialog needs the server to have credentials enabled, and a demo
// seeds a few playbooks on start, so the writable server's key is set here and both readiness timeouts
// are generous.
const serveEnv = {
  SWITCHTENDER_ENCRYPTION_KEY: "e2e-interactive-key-not-a-secret",
  SWITCHTENDER_ENCRYPTION_SALT: "e2e-interactive-salt",
};

export default defineConfig({
  testDir: ".",
  timeout: 60_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    ...channel,
  },
  projects: [
    {
      name: "smoke",
      testMatch: "smoke.spec.mjs",
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: DEMO },
    },
    {
      name: "interactive",
      testMatch: "interactive.spec.mjs",
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: SERVE },
    },
    {
      name: "assess",
      testMatch: "assess.spec.mjs",
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: SITE },
    },
    // The workflow editor runs once per server. Building a graph never leaves the browser, so the
    // demo keeps every editing control live and refuses only Run workflow and Save as template, and
    // the same spec proves both halves of that against the server each claim is about.
    {
      name: "workflow-demo",
      testMatch: "workflow.spec.mjs",
      metadata: { readOnly: true },
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: DEMO },
    },
    {
      name: "workflow-serve",
      testMatch: "workflow.spec.mjs",
      metadata: { readOnly: false },
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: SERVE },
    },
    // The editor and the runtime behind it, end to end: graphs drawn with the pointer are run for
    // real, through an approval and its deny path, and checked against what the server recorded.
    {
      name: "workflow-integration",
      testMatch: "workflow-integration.spec.mjs",
      use: { ...devices["Desktop Chrome"], ...channel, baseURL: SERVE },
    },
  ],
  webServer: [
    {
      command:
        process.env.ST_E2E_DEMO_CMD ||
        `./.bin/switchtender demo --addr 127.0.0.1:${DEMO_PORT}`,
      url: `${DEMO}/healthz`,
      // The demo seeds before it serves, and the seed runs real playbooks, two weeks of nightly
      // audits among them. In a CI container that took longer than two minutes.
      timeout: 300_000,
      // Reusing a running server keeps local iteration fast, and it means a local run can pass
      // against a binary built before the change under test. Rebuild and kill the port, or set CI=1,
      // before believing a local result: a stale server reports green for code it has never loaded.
      reuseExistingServer: !process.env.CI,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      // A fresh database each run, so the interactive suite starts from a known empty state and its
      // assertions about what it created are not confused by a previous run's leftovers.
      command:
        process.env.ST_E2E_SERVE_CMD ||
        `rm -f .bin/interactive.db && ./.bin/switchtender serve --addr 127.0.0.1:${SERVE_PORT} --db .bin/interactive.db`,
      url: `${SERVE}/healthz`,
      timeout: 60_000,
      reuseExistingServer: !process.env.CI,
      env: serveEnv,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      // The site as it deploys, with the reader built into it by npm run build:assess. A static
      // server is all the page has in production too.
      command: `python3 -m http.server ${SITE_PORT} --bind 127.0.0.1 --directory ../../../site`,
      url: `${SITE}/assess/`,
      timeout: 30_000,
      reuseExistingServer: !process.env.CI,
      stdout: "pipe",
      stderr: "pipe",
    },
  ],
});
