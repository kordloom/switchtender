# UI smoke tests

Real-browser smoke tests for the web UI, driven by Playwright against a seeded `switchtender demo`
server. They complement the dependency-free `node --test` suite in `../assets/jstest`, which drives the
same production JavaScript against a simulated DOM: that suite proves the logic and the wiring, and this
one proves the pages actually render and click through in a real browser engine, catching layout, CSS,
real event, and browser-only script breakage the simulated DOM cannot see.

## Run them

```
cd internal/ui/e2e
npm ci
npx playwright install --with-deps chromium
npm test
```

`npm test` builds the binary into `.bin/switchtender` and Playwright starts a `demo` instance on
`127.0.0.1:18777` for the run. Seeding runs a few real playbooks and takes a moment, so the readiness
timeout is generous. To point the tests at a demo you already have running, start one on that port and
Playwright reuses it (outside CI).

## What they cover

The seeded demo's overview, runs list, a run's detail, the launch dialog, the audit page, and the top
navigation, each asserting real render height so a collapsed layout fails, and each failing on any
uncaught page error or console error.
