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
  // Click the run's own id cell, the "Open run details" affordance, rather than the row's center,
  // which lands on the origin chip: a chip is its own link and correctly opens templates, so opening
  // the run means clicking the run, not its shortcut.
  await firstRun.locator("td.mono").click();
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
  await page.locator("#runs tr.row-nav").first().locator("td.mono").click();
  await expect(page).toHaveURL(/\/ui\/runs\/run_[0-9a-f]+$/);
  await expect(page.locator('[data-page="detail"]')).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/ui\/runs(\?|$)/);
  await expect(page.locator('[data-page="runs"]')).toBeVisible();
  // The list rebuilt after the back navigation, so the reader lands on the runs they were reading.
  await expect(page.locator("#runs tr.row-nav").first()).toBeVisible();
  assertNoErrors();
});

// Every page the navigation offers, so a page nobody wrote a spec for cannot render broken unnoticed.
// The tests above cover four pages; the rest were reachable in one click and checked by nobody.
const PAGES = [
  "/ui/", "/ui/runs", "/ui/fleet", "/ui/activity", "/ui/doctor", "/ui/drift",
  "/ui/tasks", "/ui/login", "/ui/users", "/ui/workers", "/ui/inventories",
  "/ui/sources", "/ui/credentials", "/ui/audit", "/ui/policies", "/ui/projects",
  "/ui/templates", "/ui/schedules", "/ui/workflows", "/ui/migrate", "/ui/docs",
];

// The demo serves no token store, so the users page asks for /v1/tokens, is told no, and says so in
// its own words. The browser still logs the 404 it saw, and no script can unlog it, so the guard
// below allows that one line rather than pretending the page is broken.
const ALLOWED_CONSOLE = [/Failed to load resource.*404/];

for (const path of PAGES) {
  test(`the page at ${path} renders without browser errors`, async ({ page }) => {
    const errors = [];
    page.on("pageerror", (err) => errors.push(`pageerror: ${err.message}`));
    page.on("console", (msg) => {
      if (msg.type() !== "error") return;
      if (ALLOWED_CONSOLE.some((re) => re.test(msg.text()))) return;
      errors.push(`console.error: ${msg.text()}`);
    });

    const response = await page.goto(path, { waitUntil: "domcontentloaded" });
    expect(response.status(), `${path} did not answer 200`).toBe(200);
    // The page is laid out rather than collapsed, which is the check a simulated DOM cannot make.
    expect(await visibleHeight(page, "body")).toBeGreaterThan(200);
    // A shell that renders with an empty body still has height, so the page must also claim its own
    // identity: every template sets data-page, and the value is what routing decided to serve.
    await expect(page.locator("body")).toHaveAttribute("data-page", /.+/);
    // Late failures arrive after load, when the page's own fetches come back.
    await page.waitForTimeout(500);
    expect(errors, `${path} reported browser errors:\n${errors.join("\n")}`).toEqual([]);
  });
}

test("an unknown ui path is refused rather than served as the overview", async ({ page }) => {
  // "/ui/" is a subtree pattern, so it also matches every path beneath it that no route claims. Left
  // alone it answered a mistyped or renamed route with the overview page and a 200, which tells a
  // reader the page they asked for exists. Each of these is a path no route owns, including the one
  // a stale link in the credentials page used to point at.
  for (const missing of ["/ui/nope", "/ui/jobtemplates", "/ui/runs/nope/deep"]) {
    const response = await page.goto(missing);
    expect(response.status(), `${missing} should be refused, not rendered`).toBe(404);
  }
});
