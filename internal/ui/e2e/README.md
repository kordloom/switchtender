# UI real-browser tests

Two Playwright suites drive the web UI in a real browser engine. They complement the dependency-free
`node --test` suite in `../assets/jstest`, which drives the same production JavaScript against a
simulated DOM: that suite proves the logic and the wiring, and these prove the pages render, click
through, and mutate in a real browser, catching layout, CSS, real event, and browser-only script
breakage the simulated DOM cannot see.

- **smoke** (`smoke.spec.mjs`) drives the seeded, read-only `switchtender demo`: rendering and
  navigation of the overview, runs list, a run's detail, the launch control, the audit page, and
  browser history, each asserting real render height so a collapsed layout fails.
- **interactive** (`interactive.spec.mjs`) drives a writable `switchtender serve` instance: it launches
  a bash run and creates a project and an inventory through the real dialogs, then reloads to confirm
  each change persisted server-side.

Both fail on any uncaught page error or console error.

## Run them

```
cd internal/ui/e2e
npm ci
npx playwright install --with-deps chromium
npm test
```

`npm test` builds the binary into `.bin/switchtender`, and Playwright starts both servers for the run: a
`demo` on `127.0.0.1:18777` and a `serve` on `127.0.0.1:18778`. Seeding the demo runs a few real
playbooks and takes a moment, so the readiness timeout is generous. Outside CI, a server already running
on either port is reused.

On a machine whose Playwright browser download is flaky but that already has Chrome, run with
`ST_E2E_CHANNEL=chrome` to drive the system browser instead of the bundled one.
