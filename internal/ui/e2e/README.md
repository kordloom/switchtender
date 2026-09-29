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
- **assess** (`assess.spec.mjs`) drives the browser assessment page, served statically from the site
  directory with the reader built into it: it drops an export named and filled beyond Latin, then
  the same export saved as UTF-16, and checks the page reads both in its worker, sets text from the
  file in fonts already on the machine, and requests nothing once it has loaded.

The first two fail on any uncaught page error or console error, and the third on any page error.

## Run them

```
cd internal/ui/e2e
npm ci
npx playwright install --with-deps chromium
npm test
```

`npm test` builds the binary into `.bin/switchtender` and the page's reader into `site/assess`, and
Playwright starts the servers for the run: a `demo` on `127.0.0.1:18777`, a `serve` on
`127.0.0.1:18778`, and the site on `127.0.0.1:18779`. Seeding the demo runs a few real playbooks and
takes a moment, so the readiness timeout is generous. Outside CI, a server already running on any of
those ports is reused.

On a machine whose Playwright browser download is flaky but that already has Chrome, run with
`ST_E2E_CHANNEL=chrome` to drive the system browser instead of the bundled one.
