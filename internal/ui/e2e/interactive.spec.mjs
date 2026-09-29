import { test, expect } from "@playwright/test";

// These drive the mutating flows the read-only demo disables, against a writable serve instance: filling
// a dialog in a real browser, submitting it, and then confirming the change actually landed, both where
// the page shows it and, for the object flows, by reloading so the assertion rests on what the server
// stored rather than on what the page optimistically drew. This is the coverage the simulated-DOM suite
// approximates with a mocked fetch and the read-only smoke suite cannot reach at all.

// attachErrorGuards records page errors and console errors, returning a function that asserts none were
// seen, so a mutating flow that half-works while logging an error surfaces here.
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

// unique returns a label unlikely to collide with another test's, so an assertion that a created object
// is present cannot pass on a namesake. The database is fresh per run, but the suffix keeps each test's
// objects distinct within it.
function unique(prefix) {
  return `${prefix}-${Math.random().toString(36).slice(2, 8)}`;
}

test("launching a bash run creates it and opens its detail", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/runs");
  await expect(page.locator('[data-page="runs"]')).toBeVisible();

  // Open the launch dialog and drive it the way an operator does: pick a tool, fill the field that tool
  // needs, and submit. Choosing bash swaps the ansible fields out for the command field.
  await page.locator("#launch-open").click();
  await expect(page.locator("#launch-modal")).toBeVisible();
  await page.locator("#launch-tool").selectOption("bash");
  await expect(page.locator("#launch-command")).toBeVisible();
  const marker = unique("echo-e2e");
  await page.locator("#launch-command").fill(`echo ${marker}`);

  // Submitting posts the run and navigates to the new run's detail, which is how the operator lands on
  // what they just started. That navigation is the server's own id coming back, so reaching it proves
  // the run was created.
  await page.locator('#launch-form button[type="submit"]').click();
  await expect(page).toHaveURL(/\/ui\/runs\/run_[0-9a-f]+$/, { timeout: 30_000 });
  await expect(page.locator('[data-page="detail"]')).toBeVisible();
  await expect(page.locator("#run-header")).toBeVisible();
  const runURL = page.url();

  // And the run is really in the list, not only reachable by its own url: reloading the runs page shows
  // a row, and clicking the first one lands back on the run that was just created (it is the newest, so
  // it sorts first).
  const runId = runURL.split("/").pop();
  await page.goto("/ui/runs");
  const firstRow = page.locator("#runs tr.row-nav").first();
  await expect(firstRow).toBeVisible();
  await firstRow.click();
  await expect(page).toHaveURL(new RegExp(`/ui/runs/${runId}$`));
  assertNoErrors();
});

test("creating a project stores it and shows it in the list", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/projects");
  await expect(page.locator('[data-page="projects"]')).toBeVisible();

  const name = unique("project");
  await page.locator("#project-open").click();
  await expect(page.locator("#project-modal")).toBeVisible();
  await page.locator("#project-name").fill(name);
  await page.locator("#project-repo").fill("https://example.com/e2e.git");
  await page.locator('#project-form button[type="submit"]').click();

  // The new project appears on the page.
  await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 15_000 });

  // It is really stored, not only drawn: a reload reads it back from the server.
  await page.reload();
  await expect(page.getByText(name, { exact: false }).first()).toBeVisible();
  assertNoErrors();
});

test("creating an inventory stores it and shows it in the list", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await page.goto("/ui/inventories");
  await expect(page.locator('[data-page="inventories"]')).toBeVisible();

  const name = unique("inventory");
  await page.locator("#inventory-open").click();
  await expect(page.locator("#inventory-modal")).toBeVisible();
  await page.locator("#inv-name").fill(name);
  await page.locator("#inv-content").fill("[all]\nweb-e2e-1\n");
  await page.locator('#inventory-form button[type="submit"]').click();

  await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 15_000 });
  await page.reload();
  await expect(page.getByText(name, { exact: false }).first()).toBeVisible();
  assertNoErrors();
});
