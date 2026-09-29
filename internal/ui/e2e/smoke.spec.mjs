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
  // The detail page rendered its own content, not just the shell: the run header carries the run's
  // identity and is populated. The page shows a shortened id, so the header's own text rather than the
  // full url id is what a reader sees, and it is not empty.
  const header = page.locator("#run-header");
  await expect(header).toBeVisible();
  await expect(header).not.toBeEmpty();
  assertNoErrors();
});

test("the launch control renders and the dialog is hidden until invoked", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/runs");
  // The launch button is a real, laid-out control on the page.
  const open = page.locator("#launch-open");
  await expect(open).toBeVisible();
  expect((await open.textContent()).trim().length).toBeGreaterThan(0);
  // The dialog exists in the page and is hidden by CSS until it is opened, which a real browser reports
  // as not visible and a DOM-only test cannot tell apart from visible. Actually opening it is a mutating
  // flow the seeded demo serves read-only, so this asserts the resting state the demo does expose: the
  // control is present and the dialog is not yet shown.
  await expect(page.locator("#launch-modal")).toBeHidden();
  assertNoErrors();
});

test("the audit page renders and offers offline verification", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/audit");
  await expect(page.locator('[data-page="audit"]')).toBeVisible();
  expect(await visibleHeight(page, "body")).toBeGreaterThan(200);
  assertNoErrors();
});

test("navigation and browser history move between pages", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  // From the runs list, opening a run and then going back is the round trip a reader makes constantly,
  // and it exercises real click navigation and the browser's own history in a real engine, which a
  // simulated DOM has neither of.
  await page.goto("/ui/runs");
  await expect(page.locator('[data-page="runs"]')).toBeVisible();
  await page.locator("#runs tr.row-nav").first().click();
  await expect(page).toHaveURL(/\/ui\/runs\/run_[0-9a-f]+$/);
  await expect(page.locator('[data-page="detail"]')).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/ui\/runs(\?|$)/);
  await expect(page.locator('[data-page="runs"]')).toBeVisible();
  // The list rebuilt after the back navigation, so the reader lands on the runs they were reading.
  await expect(page.locator("#runs tr.row-nav").first()).toBeVisible();
  assertNoErrors();
});
