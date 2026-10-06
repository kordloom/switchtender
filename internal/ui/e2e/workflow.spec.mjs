import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";

// The workflow editor, driven the way a visitor drives it: real pointer gestures on the canvas, the
// real step dialog, and real downloads. It runs twice, once against the read-only demo and once
// against a writable serve instance, because the demo makes a promise worth checking: building a
// graph only rewrites the page's own state, so every editing control stays live there, and only the
// two controls that send the graph to the server are refused.
//
// Every test starts from a fresh browser context, so the editor opens on its seeded sample: provision
// fans out to configure and migrate-db, and both feed smoke-test.

// attachErrorGuards records page errors and console errors, returning a function that asserts none
// were seen.
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

// readOnly reports whether the running project drives the read-only demo.
function readOnly() {
  return test.info().project.metadata.readOnly === true;
}

// openEditor loads the workflow page, waits for the sample graph, and checks the server is the kind
// the project says it is, so a demo assertion can never pass against a writable server.
async function openEditor(page) {
  await page.goto("/ui/workflows");
  await expect(page.locator('[data-page="workflows"]')).toBeVisible();
  if (readOnly()) {
    await expect(page.locator("body")).toHaveAttribute("data-readonly", "true");
  } else {
    await expect(page.locator("body")).not.toHaveAttribute("data-readonly", "true");
  }
  await expect(page.locator(".wf-node")).toHaveCount(4);
  await page.locator("#wf-canvas").scrollIntoViewIfNeeded();
}

// step returns the card for the step with the given name.
function step(page, name) {
  return page.locator(".wf-node").filter({
    has: page.locator(".wf-node-name", { hasText: new RegExp(`^${name}$`) }),
  });
}

// link returns the hit path the editor draws for the dependency from one step into another. One is
// drawn per edge, labeled with both names, so it is what a reader's link looks like on the canvas.
function link(page, from, to) {
  return page.locator(`.wf-edge-hit[aria-label^="Dependency link, ${from} into ${to}."]`);
}

// fillStep fills the open step dialog for a bash step and saves it.
async function fillStep(page, name, command) {
  const dialog = page.locator("#wf-step-modal");
  await expect(dialog).toBeVisible();
  await page.locator("#wf-step-name").fill(name);
  await page.locator("#wf-step-tool").selectOption("bash");
  await page.locator("#wf-step-command").fill(command);
  await page.locator('#wf-step-form button[type="submit"]').click();
  await expect(dialog).toBeHidden();
}

// dragLink draws a dependency the way a reader does: press on the source step's right dot, drag, and
// release over the target step.
async function dragLink(page, from, to) {
  await page.locator("#wf-canvas").scrollIntoViewIfNeeded();
  const dot = await step(page, from).locator(".wf-out").boundingBox();
  const target = await step(page, to).boundingBox();
  await page.mouse.move(dot.x + dot.width / 2, dot.y + dot.height / 2);
  await page.mouse.down();
  await page.mouse.move(target.x + target.width / 2, target.y + target.height / 2, { steps: 10 });
  await page.mouse.up();
}

// insideCanvas asserts a step's card is drawn within the canvas viewport, where a reader sees it.
async function insideCanvas(page, name) {
  const canvas = await page.locator("#wf-canvas").boundingBox();
  const card = await step(page, name).boundingBox();
  expect(card, `${name} has no box, so it is not rendered`).not.toBeNull();
  expect(card.x).toBeGreaterThanOrEqual(canvas.x);
  expect(card.y).toBeGreaterThanOrEqual(canvas.y);
  expect(card.x + card.width).toBeLessThanOrEqual(canvas.x + canvas.width);
  expect(card.y + card.height).toBeLessThanOrEqual(canvas.y + canvas.height);
}

test("the toolbar's Add step adds a step that renders on the canvas", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await page.locator("#wf-add").click();
  await expect(page.locator("#wf-step-name")).toBeFocused();
  // Saving a step works in the demo, so the dialog must not say it is disabled there.
  await expect(page.locator("#wf-step-modal")).not.toContainText("Disabled");
  await fillStep(page, "lint", "make lint");

  await expect(page.locator(".wf-node")).toHaveCount(5);
  const added = step(page, "lint");
  await expect(added).toBeVisible();
  await expect(added.locator(".wf-tool")).toHaveText("bash");
  await expect(added.locator(".wf-node-target")).toHaveText("make lint");
  await insideCanvas(page, "lint");
  assertNoErrors();
});

test("clicking a step opens it for editing and saving changes the card", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await step(page, "configure").locator(".wf-node-target").click();
  await expect(page.locator("#wf-step-modal")).toBeVisible();
  await expect(page.locator("#wf-step-name")).toHaveValue("configure");
  await expect(page.locator("#wf-step-playbook")).toHaveValue("site.yml");
  await page.locator("#wf-step-playbook").fill("configure.yml");
  await page.locator('#wf-step-form button[type="submit"]').click();
  await expect(page.locator("#wf-step-modal")).toBeHidden();

  await expect(step(page, "configure").locator(".wf-node-target")).toHaveText("configure.yml");
  // The graph is the reader's own now, so it no longer says it is the sample.
  await expect(page.locator("#wf-sample-note")).toBeHidden();
  assertNoErrors();
});

test("dragging from a step's right dot links it to another step", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(4);

  // Two steps of the sample itself, the graph every visitor first meets.
  await dragLink(page, "configure", "migrate-db");
  await expect(link(page, "configure", "migrate-db")).toHaveCount(1);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(5);
  await expect(page.locator("#status")).toHaveText("migrate-db now waits for configure.");

  // And into a step the reader just added.
  await page.locator("#wf-add").click();
  await fillStep(page, "notify", "echo done");
  await dragLink(page, "smoke-test", "notify");
  await expect(link(page, "smoke-test", "notify")).toHaveCount(1);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(6);
  assertNoErrors();
});

test("selecting a link and pressing Delete removes it", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await link(page, "provision", "configure").focus();
  await expect(page.locator("#status")).toHaveText("Dependency selected. Press Delete to remove it.");
  await page.keyboard.press("Delete");
  await expect(link(page, "provision", "configure")).toHaveCount(0);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(3);
  await expect(page.locator("#status")).toHaveText("Dependency removed.");
  assertNoErrors();
});

test("clearing the sample opens an empty canvas with both ways in, each live", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  // The sample note says to clear the canvas, so it carries the control that does.
  await page.locator("#wf-sample-note").getByRole("button", { name: "Clear canvas" }).click();
  await expect(page.locator(".wf-node")).toHaveCount(0);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(0);
  await expect(page.locator("#wf-sample-note")).toBeHidden();
  const hint = page.locator(".wf-hint");
  await expect(hint).toBeVisible();
  await expect(hint).toContainText("Nothing on the canvas yet");
  const first = hint.getByRole("button", { name: "Add your first step" });
  const pattern = hint.getByRole("button", { name: "Start from a pattern" });
  // The empty state sits in the middle of the canvas, where the graph would be.
  const canvas = await page.locator("#wf-canvas").boundingBox();
  const box = await hint.boundingBox();
  expect(Math.abs(box.x + box.width / 2 - (canvas.x + canvas.width / 2))).toBeLessThan(4);
  expect(Math.abs(box.y + box.height / 2 - (canvas.y + canvas.height / 2))).toBeLessThan(4);
  // The control the clear removed handed focus on, so a keyboard user is not dropped.
  await expect(first).toBeFocused();
  await expect(page.locator("#wf-canvas-add")).toBeVisible();

  // Both ways in are pressed with the mouse, the way a reader meets them.
  await first.click();
  await expect(page.locator("#wf-step-modal")).toBeVisible();
  await fillStep(page, "first", "echo first");
  await expect(step(page, "first")).toBeVisible();
  await expect(hint).toBeHidden();

  await step(page, "first").locator(".wf-node-del").click();
  await expect(hint).toBeVisible();
  await pattern.click();
  await expect(page.locator("#wf-wizard-modal")).toBeVisible();
  await page.locator(".wf-pattern", { hasText: "Fan out, then gate" }).click();
  await expect(page.locator(".wf-node")).toHaveCount(5);
  await expect(hint).toBeHidden();
  assertNoErrors();
});

test("the canvas's own Add step adds a step from its corner", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  // It sits inside the canvas, in its top right corner, clear of the zoom controls.
  const add = page.locator("#wf-canvas-add");
  await expect(add).toBeVisible();
  const canvas = await page.locator("#wf-canvas").boundingBox();
  const box = await add.boundingBox();
  expect(box.x + box.width).toBeLessThanOrEqual(canvas.x + canvas.width);
  expect(box.x + box.width).toBeGreaterThan(canvas.x + canvas.width - 40);
  expect(box.y).toBeGreaterThanOrEqual(canvas.y);
  expect(box.y).toBeLessThan(canvas.y + 40);

  await add.click();
  await expect(page.locator("#wf-step-title")).toHaveText("Add a step");
  await expect(page.locator("#wf-step-after")).toBeHidden();
  await fillStep(page, "lint", "make lint");
  await expect(page.locator(".wf-node")).toHaveCount(5);
  await insideCanvas(page, "lint");
  // A step added from the corner is free standing until the reader links it.
  await expect(page.locator(".wf-edge-hit")).toHaveCount(4);
  // Focus went back to the control that opened the dialog, which still exists.
  await expect(add).toBeFocused();
  assertNoErrors();
});

test("a step's + shows on hover and adds a step after it, already linked", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  const smoke = step(page, "smoke-test");
  const plus = smoke.locator(".wf-node-add");
  await expect(plus).toHaveAttribute("aria-label", "Add a step after smoke-test");
  await page.mouse.move(1, 1);
  await expect(plus).toHaveCSS("opacity", "0");
  await smoke.hover();
  await expect(plus).toHaveCSS("opacity", "1");
  await plus.click();

  await expect(page.locator("#wf-step-modal")).toBeVisible();
  await expect(page.locator("#wf-step-after")).toHaveText(
    "Runs after smoke-test. Saving links it from there.");
  await fillStep(page, "notify", "echo shipped");

  await expect(step(page, "notify")).toBeVisible();
  await expect(link(page, "smoke-test", "notify")).toHaveCount(1);
  await expect(page.locator("#status")).toHaveText("notify now waits for smoke-test.");
  // It lands one column to the right, on the same row, and the canvas pans to keep it in view.
  const from = await smoke.boundingBox();
  const to = await step(page, "notify").boundingBox();
  expect(to.x).toBeGreaterThan(from.x + from.width);
  expect(Math.abs(to.y - from.y)).toBeLessThan(2);
  await insideCanvas(page, "notify");
  await insideCanvas(page, "smoke-test");
  assertNoErrors();
});

test("a focused step shows its +, and A or the + adds after it from the keyboard", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  const provision = step(page, "provision");
  await provision.focus();
  await expect(provision.locator(".wf-node-add")).toHaveCSS("opacity", "1");
  await page.keyboard.press("a");
  await expect(page.locator("#wf-step-after")).toHaveText(
    "Runs after provision. Saving links it from there.");
  await page.keyboard.type("lint");
  await page.locator("#wf-step-tool").selectOption("bash");
  await page.locator("#wf-step-command").fill("make lint");
  // Enter in a text field submits the dialog, the keyboard's way of pressing Save step.
  await page.locator("#wf-step-inventory").press("Enter");
  await expect(page.locator("#wf-step-modal")).toBeHidden();
  await expect(link(page, "provision", "lint")).toHaveCount(1);
  // Focus lands on the step just added, so the next key acts on it.
  await expect(step(page, "lint")).toBeFocused();

  // Tab walks from a card to its delete control and then to its +, and Enter on the + adds after
  // that step rather than opening the card for editing.
  await page.keyboard.press("Tab");
  await expect(step(page, "lint").locator(".wf-node-del")).toBeFocused();
  await page.keyboard.press("Tab");
  const plus = step(page, "lint").locator(".wf-node-add");
  await expect(plus).toBeFocused();
  await expect(plus).toHaveCSS("opacity", "1");
  await page.keyboard.press("Enter");
  await expect(page.locator("#wf-step-title")).toHaveText("Add a step");
  await expect(page.locator("#wf-step-after")).toHaveText("Runs after lint. Saving links it from there.");
  await page.keyboard.press("Escape");
  await expect(page.locator("#wf-step-modal")).toBeHidden();
  await expect(plus).toBeFocused();
  assertNoErrors();
});

// expectDialogInView asserts a dialog's card lies wholly on screen and nothing is drawn over its close,
// then closes it with a real click.
async function expectDialogInView(page, modal, close) {
  const card = page.locator(`${modal} .modal-card`);
  await expect(card).toBeVisible();
  const box = await card.boundingBox();
  const view = page.viewportSize();
  expect(box.y, `${modal} opened above the top of the screen`).toBeGreaterThanOrEqual(0);
  expect(box.y + box.height, `${modal} opened past the bottom of the screen`)
    .toBeLessThanOrEqual(view.height);
  const onTop = await page.locator(close).evaluate((el) => {
    const r = el.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return hit === el || el.contains(hit);
  });
  expect(onTop, `something is drawn over the close of ${modal}`).toBe(true);
  await page.locator(close).click();
  await expect(page.locator(modal)).toBeHidden();
}

test("dialogs open wholly on screen above the page chrome, at any scroll", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);
  // Readers scroll down to work the graph, so that is where the step dialog has to open in view.
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await page.waitForTimeout(300);
  await step(page, "configure").locator(".wf-node-target").click();
  await expectDialogInView(page, "#wf-step-modal", "#wf-step-close");

  // The pattern chooser is the tallest dialog on the page, tall enough to reach the top bar.
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.waitForTimeout(300);
  await page.locator("#wf-wizard-open").click();
  await expectDialogInView(page, "#wf-wizard-modal", "#wf-wizard-close");
  assertNoErrors();
});

test("a step's delete control removes it and every link touching it", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await step(page, "migrate-db").locator(".wf-node-del").click();
  await expect(step(page, "migrate-db")).toHaveCount(0);
  await expect(page.locator(".wf-node")).toHaveCount(3);
  await expect(page.locator(".wf-edge-hit")).toHaveCount(2);
  await expect(link(page, "provision", "configure")).toHaveCount(1);
  await expect(link(page, "configure", "smoke-test")).toHaveCount(1);
  assertNoErrors();
});

test("starting from a pattern lays out its steps and links", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await page.locator("#wf-wizard-open").click();
  const wizard = page.locator("#wf-wizard-modal");
  await expect(wizard).toBeVisible();
  // The canvas holds the sample, so the chooser says what choosing will replace.
  await expect(page.locator("#wf-wizard-warn")).toContainText("already holds 4 steps");
  await page.locator("#wf-wizard-tool").selectOption("bash");
  await page.locator(".wf-pattern", { hasText: "One after another" }).click();
  await expect(wizard).toBeHidden();

  await expect(page.locator(".wf-node")).toHaveCount(3);
  for (const name of ["build", "test", "deploy"]) {
    await expect(step(page, name).locator(".wf-tool")).toHaveText("bash");
  }
  await expect(link(page, "build", "test")).toHaveCount(1);
  await expect(link(page, "test", "deploy")).toHaveCount(1);
  await expect(page.locator("#status")).toContainText("Laid out 3 steps");
  assertNoErrors();
});

test("JSON export downloads the graph as the pipeline it would run", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.locator("#wf-export-json").click(),
  ]);
  expect(download.suggestedFilename()).toBe("release-pipeline.json");
  const doc = JSON.parse(readFileSync(await download.path(), "utf8"));
  expect(doc.name).toBe("Release pipeline");
  expect(doc.steps.map((s) => s.name)).toEqual(["provision", "configure", "migrate-db", "smoke-test"]);
  const byName = Object.fromEntries(doc.steps.map((s) => [s.name, s]));
  expect(byName.configure.depends_on).toEqual(["provision"]);
  expect(byName["smoke-test"].depends_on.sort()).toEqual(["configure", "migrate-db"]);
  await expect(page.locator("#status")).toHaveText("Exported 4 steps to release-pipeline.json.");
  assertNoErrors();
});

test("the demo refuses Run workflow and Save as template, and only those", async ({ page }) => {
  test.skip(!readOnly(), "the refusal is the read-only demo's");
  const assertNoErrors = attachErrorGuards(page);
  const writes = [];
  page.on("request", (req) => {
    if (req.method() !== "GET" && /\/v1\/(pipelines|templates)/.test(req.url())) {
      writes.push(`${req.method()} ${req.url()}`);
    }
  });
  await openEditor(page);

  // A pattern makes the graph the reader's own, so what refuses below is the demo and not the guard
  // that holds the untouched sample back.
  await page.locator("#wf-wizard-open").click();
  await page.locator(".wf-pattern", { hasText: "One after another" }).click();
  await expect(page.locator("#wf-sample-note")).toBeHidden();

  const before = page.url();
  for (const id of ["wf-run", "wf-save-template"]) {
    await expect(page.locator(`#${id}`)).toHaveAttribute("data-mutates", "true");
    await page.locator(`#${id}`).click();
  }
  await page.waitForTimeout(500);
  expect(writes, "the demo sent the graph to the server").toEqual([]);
  expect(page.url()).toBe(before);
  await expect(page.locator("#status")).not.toContainText("Starting workflow");
  await expect(page.locator("#status")).not.toContainText("Saving workflow template");
  assertNoErrors();
});

test("a writable server runs the graph the editor built", async ({ page }) => {
  test.skip(readOnly(), "the demo refuses to run anything");
  const assertNoErrors = attachErrorGuards(page);
  await openEditor(page);

  await page.locator("#wf-wizard-open").click();
  await page.locator("#wf-wizard-tool").selectOption("bash");
  await page.locator(".wf-pattern", { hasText: "One after another" }).click();
  await page.locator("#wf-name").fill("e2e workflow");
  await page.locator("#wf-run").click();
  await expect(page).toHaveURL(/\/ui\/runs\/run_[0-9a-f]+$/, { timeout: 30_000 });
  await expect(page.locator("#run-header")).toBeVisible();
  assertNoErrors();
});
