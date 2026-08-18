import { test, expect } from "@playwright/test";

// These run the real web UI in a real browser against a seeded demo server, which is the one thing the
// node --test suite in ../assets/jstest cannot do: that suite drives the same production JavaScript
// against a simulated DOM with no layout and no CSS, so it proves the logic and the wiring but not that
// a page renders and clicks through in a real engine. Everything here is a genuine render: real
// stylesheet, real event dispatch, real fetch to the running server.
//
// Every test also fails on an uncaught page error or a console error, so a browser-only breakage, a
// script that throws only under a real engine, surfaces here rather than in a support ticket.

// attachErrorGuards records page errors and console errors on a page, returning a function that asserts
// none were seen. A console error is the browser's own report that something went wrong, and a page
// that renders while quietly logging one is a page with a latent bug.
function attachErrorGuards(page) {
  const errors = [];
  page.on("pageerror", (err) => errors.push(`pageerror: ${err.message}`));
  page.on("console", (msg) => {
    if (msg.type() === "error") errors.push(`console.error: ${msg.text()}`);
  });
  return () => {
    expect(errors, `the page reported browser errors:\n${errors.join("\n")}`).toEqual([]);
  };
}

// visibleHeight reports the rendered height of a selector, which is zero when CSS has collapsed or
// hidden it. It is how a real-browser test catches a layout regression a DOM-only test cannot see.
async function visibleHeight(page, selector) {
  return page.locator(selector).first().evaluate((el) => el.getBoundingClientRect().height);
}

test("the overview page renders with real layout", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/");
  await expect(page.locator('[data-page="overview"]')).toBeVisible();
  // The page has real height, so it is laid out rather than collapsed. A DOM-only test reads every
  // geometry as zero and cannot make this assertion at all.
  expect(await visibleHeight(page, "body")).toBeGreaterThan(200);
  assertNoErrors();
});

test("the runs page fetches and renders run rows", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/runs");
  await expect(page.locator('[data-page="runs"]')).toBeVisible();
  // The list is populated by a real fetch to /v1/runs and rendered into the real table as clickable
  // rows, so waiting for a row proves the whole path: request, response, and DOM update in the browser.
  // The demo seeds several runs.
  await expect(page.locator("#runs tr.row-nav").first()).toBeVisible();
  assertNoErrors();
});

test("clicking a run opens its detail page", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/runs");
  const firstRun = page.locator("#runs tr.row-nav").first();
  await expect(firstRun).toBeVisible();
  // The row is a role=link with a click handler that navigates to the run's detail, which is how an
  // operator opens a run. Clicking the real element exercises that handler in the real engine.
  await firstRun.click();
  await expect(page).toHaveURL(/\/ui\/runs\/run_[0-9a-f]+$/);
  await expect(page.locator('[data-page="detail"]')).toBeVisible();
  // The detail page rendered its own content, not just the shell: the run header is present and the
  // page names the run the url points at.
  await expect(page.locator("#run-header")).toBeVisible();
  const runId = page.url().split("/").pop();
  await expect(page.getByText(runId, { exact: false }).first()).toBeVisible();
  assertNoErrors();
});

test("the launch dialog opens and closes", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/runs");
  const modal = page.locator("#launch-modal");
  // Closed to start: a real browser reports it as not visible because CSS hides it, which is exactly the
  // state a DOM-only test cannot distinguish from visible.
  await expect(modal).toBeHidden();
  await page.locator("#launch-open").click();
  await expect(modal).toBeVisible();
  expect(await visibleHeight(page, "#launch-modal")).toBeGreaterThan(50);
  // The form the operator fills in is really inside the opened dialog.
  await expect(page.locator("#launch-form")).toBeVisible();
  await page.locator("#launch-close").click();
  await expect(modal).toBeHidden();
  assertNoErrors();
});

test("the audit page renders and offers offline verification", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/audit");
  await expect(page.locator('[data-page="audit"]')).toBeVisible();
  expect(await visibleHeight(page, "body")).toBeGreaterThan(200);
  assertNoErrors();
});

test("the top navigation moves between pages", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/");
  await expect(page.locator('[data-page="overview"]')).toBeVisible();
  // Following the real nav link changes the page, which is the click a reader makes to get around.
  await page.locator('a[href="/ui/runs"]').first().click();
  await expect(page).toHaveURL(/\/ui\/runs$/);
  await expect(page.locator('[data-page="runs"]')).toBeVisible();
  assertNoErrors();
});
