import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";

// The workflow editor and the workflow runtime behind it, driven end to end against a writable
// server. workflow.spec.mjs proves each editing gesture; this file proves what a person actually
// does with them: draw a graph, run it, and have the server do exactly what the picture said.
//
// Every graph here is built with real pointer gestures and dialogs, then run for real. What a test
// asserts about a run comes from the server's own records, never from the page, so a drawing that
// disagrees with what executes fails here.
//
// The server is shared with the other suites and their runs, so every workflow takes a name unique
// to its test and every lookup filters by it.

test.use({ viewport: { width: 1440, height: 1000 } });

// RUN makes workflow names unique to this execution of the suite.
const RUN = Date.now().toString(36);

// attachErrorGuards records page errors and console errors, returning a function that asserts none
// were seen.
function attachErrorGuards(page) {
  const errors = [];
  page.on("pageerror", (err) => errors.push(`pageerror: ${err.message}`));
  page.on("console", (msg) => {
    if (msg.type() === "error") errors.push(`console.error: ${msg.text()}`);
  });
  return () => expect(errors, `the page reported browser errors:\n${errors.join("\n")}`).toEqual([]);
}

// step returns the card for the step with the given name.
function step(page, name) {
  return page.locator(".wf-node").filter({
    has: page.locator(".wf-node-name", { hasText: new RegExp(`^${name}$`) }),
  });
}

// openEmptyEditor loads the editor and clears its seeded sample, so a test starts from nothing.
async function openEmptyEditor(page) {
  await page.goto("/ui/workflows");
  await expect(page.locator(".wf-node")).toHaveCount(4);
  await page.locator("#wf-canvas").scrollIntoViewIfNeeded();
  await page.locator("#wf-sample-note").getByRole("button", { name: "Clear canvas" }).click();
  await expect(page.locator(".wf-node")).toHaveCount(0);
}

// openSample loads the editor on its seeded sample.
async function openSample(page) {
  await page.goto("/ui/workflows");
  await expect(page.locator(".wf-node")).toHaveCount(4);
  await page.locator("#wf-canvas").scrollIntoViewIfNeeded();
}

// fillStep fills the open step dialog from fields and submits it. Only the fields given are set.
async function fillStep(page, fields) {
  await expect(page.locator("#wf-step-modal")).toBeVisible();
  if (fields.kind) await page.locator("#wf-step-kind").selectOption(fields.kind);
  if (fields.name !== undefined) await page.locator("#wf-step-name").fill(fields.name);
  if (fields.tool) await page.locator("#wf-step-tool").selectOption(fields.tool);
  if (fields.command !== undefined) await page.locator("#wf-step-command").fill(fields.command);
  if (fields.description !== undefined) await page.locator("#wf-step-description").fill(fields.description);
  if (fields.retries !== undefined) await page.locator("#wf-step-retries").fill(String(fields.retries));
  if (fields.continueOnFailure) await page.locator("#wf-step-continue").check();
  await page.locator('#wf-step-form button[type="submit"]').click();
}

// bashStep describes a bash step for fillStep.
function bashStep(name, command) {
  return { name, tool: "bash", command };
}

// addFirst adds the first step on an empty canvas from the empty canvas's own button.
async function addFirst(page, fields) {
  await page.locator("#wf-hint-add").click();
  await fillStep(page, fields);
  await expect(page.locator("#wf-step-modal")).toBeHidden();
}

// addAfter adds a step after another, from the + on the other's card, already linked.
async function addAfter(page, after, fields) {
  await step(page, after).hover();
  await step(page, after).locator(".wf-node-add").click();
  await fillStep(page, fields);
  await expect(page.locator("#wf-step-modal")).toBeHidden();
}

// addFromToolbar adds an unlinked step from the toolbar.
async function addFromToolbar(page, fields) {
  await page.locator("#wf-add").click();
  await fillStep(page, fields);
  await expect(page.locator("#wf-step-modal")).toBeHidden();
}

// dragLink draws a dependency, or a deny path from an approval step's red dot, with the pointer:
// press on the source's dot, drag, and release over the target card.
async function dragLink(page, from, to, dot = ".wf-out") {
  await page.locator("#wf-canvas").scrollIntoViewIfNeeded();
  const source = await step(page, from).locator(dot).boundingBox();
  const target = await step(page, to).boundingBox();
  await page.mouse.move(source.x + source.width / 2, source.y + source.height / 2);
  await page.mouse.down();
  await page.mouse.move(target.x + target.width / 2, target.y + target.height / 2, { steps: 10 });
  await page.mouse.up();
}

// snapshot reads the graph the page draws: every step's name and every link's label, sorted, so two
// snapshots compare equal when the pictures say the same thing.
async function snapshot(page) {
  const names = (await page.locator(".wf-node .wf-node-name").allInnerTexts()).sort();
  const edges = await page.locator(".wf-edge-hit").evaluateAll((els) =>
    els.map((el) => (el.getAttribute("aria-label") || "").replace(/\.\s.*$/, "")).sort());
  return { names, edges };
}

// dep is the label the page gives a dependency from one step into another.
function dep(from, to) {
  return `Dependency link, ${from} into ${to}`;
}

// runStatus reads a run's status from the server.
async function runStatus(request, id) {
  const res = await request.get(`/v1/runs/${id}`);
  expect(res.ok(), `GET /v1/runs/${id} answered ${res.status()}`).toBe(true);
  return (await res.json()).status;
}

// stepRuns reads every attempt of every step of a workflow, in the order the server lists them.
async function stepRuns(request, id) {
  const res = await request.get(`/v1/runs/${id}/steps`);
  expect(res.ok(), `GET /v1/runs/${id}/steps answered ${res.status()}`).toBe(true);
  return (await res.json()).steps;
}

// waitForStatus waits until the workflow is in one of the given states and returns it.
async function waitForStatus(request, id, wanted, timeout = 60_000) {
  let seen = "";
  await expect.poll(async () => (seen = await runStatus(request, id)), {
    timeout, message: `workflow ${id} was ${seen}, wanted ${wanted.join(" or ")}`,
  }).toMatch(new RegExp(`^(${wanted.join("|")})$`));
  return seen;
}

// runFromEditor presses Run workflow and returns the id of the workflow it started, read from the
// run page the editor opens.
async function runFromEditor(page, name) {
  await page.locator("#wf-name").fill(name);
  await page.locator("#wf-run").click();
  await page.waitForURL(/\/ui\/runs\/run_[0-9a-f]+/);
  return /\/ui\/runs\/(run_[0-9a-f]+)/.exec(page.url())[1];
}

// decide answers a waiting approval step from the runs page, as an approver would: find the
// workflow's card, press Approve or Deny, and confirm the reason dialog without a reason.
async function decide(page, workflow, answer) {
  await page.goto("/ui/runs");
  const card = page.locator(".approval-step", { hasText: workflow });
  await card.getByRole("button", { name: answer, exact: true }).click();
  await page.locator("#reason-go").click();
}

// startedBefore asserts that one step's every attempt ended before another's first began.
function endedBefore(rows, first, second) {
  const a = rows.filter((r) => r.step_name === first);
  const b = rows.filter((r) => r.step_name === second);
  expect(a.length, `${first} never ran`).toBeGreaterThan(0);
  expect(b.length, `${second} never ran`).toBeGreaterThan(0);
  const ended = a.map((r) => r.ended_at || r.started_at).sort().pop();
  const began = b.map((r) => r.started_at).sort()[0];
  expect(ended <= began, `${first} ended ${ended}, after ${second} began ${began}`).toBe(true);
}

// drawReleaseWorkflow draws build, test, an approval step, deploy after it, and a page-oncall step
// on the approval's deny path, the shape a release takes.
async function drawReleaseWorkflow(page) {
  await openEmptyEditor(page);
  await addFirst(page, bashStep("build", "echo building && sleep 1"));
  await addAfter(page, "build", bashStep("test", "echo testing"));
  await addAfter(page, "test", { name: "approve-release", kind: "approval", description: "Ship it?" });
  await addAfter(page, "approve-release", bashStep("deploy", "echo deploying"));
  await addFromToolbar(page, bashStep("page-oncall", "echo paging the on-call"));
  await dragLink(page, "approve-release", "page-oncall", ".wf-out-deny");
  expect(await snapshot(page)).toEqual({
    names: ["approve-release", "build", "deploy", "page-oncall", "test"],
    edges: [dep("approve-release", "deploy"), dep("build", "test"), dep("test", "approve-release"),
      "Deny path link, approve-release into page-oncall"].sort(),
  });
}

test("a workflow drawn from an empty canvas runs in order through its approval", async ({ page, request }) => {
  const assertNoErrors = attachErrorGuards(page);
  const name = `release-${RUN}`;
  await drawReleaseWorkflow(page);
  const id = await runFromEditor(page, name);

  await waitForStatus(request, id, ["pending_approval"]);
  const waiting = await stepRuns(request, id);
  expect(waiting.map((r) => r.step_name)).not.toContain("deploy");

  await decide(page, name, "Approve");
  await waitForStatus(request, id, ["succeeded"]);
  const rows = await stepRuns(request, id);
  const ran = (s) => rows.filter((r) => r.step_name === s).map((r) => r.status);
  expect(ran("build")).toEqual(["succeeded"]);
  expect(ran("test")).toEqual(["succeeded"]);
  expect(ran("deploy")).toEqual(["succeeded"]);
  expect(rows.map((r) => r.step_name), "the deny path ran though the approval was granted")
    .not.toContain("page-oncall");
  endedBefore(rows, "build", "test");
  endedBefore(rows, "test", "deploy");
  assertNoErrors();
});

test("denying the approval runs the deny path and never the deploy", async ({ page, request }) => {
  const assertNoErrors = attachErrorGuards(page);
  const name = `denied-${RUN}`;
  await drawReleaseWorkflow(page);
  const id = await runFromEditor(page, name);
  await waitForStatus(request, id, ["pending_approval"]);

  await page.goto("/ui/runs");
  await expect(page.locator(".approval-step", { hasText: name })).toContainText("Denying runs page-oncall");
  await decide(page, name, "Deny");
  await waitForStatus(request, id, ["succeeded", "failed", "rejected", "canceled"]);
  const rows = await stepRuns(request, id);
  const ran = (s) => rows.filter((r) => r.step_name === s).map((r) => r.status);
  expect(ran("page-oncall"), "the deny path did not run").toEqual(["succeeded"]);
  expect(ran("deploy"), "the deploy ran though the approval was denied").toEqual([]);
  assertNoErrors();
});

test("retries and continue on failure reach the server and behave as drawn", async ({ page, request }, testInfo) => {
  const assertNoErrors = attachErrorGuards(page);
  // The flaky step fails twice and passes on its third attempt, counted in a file so each attempt
  // sees how many came before it.
  const counter = testInfo.outputPath("attempts");
  const flaky = `n=$(cat '${counter}' 2>/dev/null || echo 0); echo $((n+1)) > '${counter}'; [ $n -ge 2 ]`;
  await openEmptyEditor(page);
  await addFirst(page, { ...bashStep("flaky", flaky), retries: 2 });
  await addAfter(page, "flaky", bashStep("after-flaky", "echo after"));
  await addFromToolbar(page, { ...bashStep("always-fails", "exit 7"), continueOnFailure: true });
  await addAfter(page, "always-fails", bashStep("runs-anyway", "echo still here"));
  await addFromToolbar(page, bashStep("hard-fail", "exit 3"));
  await addAfter(page, "hard-fail", bashStep("never-runs", "echo should not run"));

  const id = await runFromEditor(page, `flags-${RUN}`);
  await waitForStatus(request, id, ["succeeded", "failed"]);
  const rows = await stepRuns(request, id);
  const attempts = (s) => rows.filter((r) => r.step_name === s)
    .sort((a, b) => a.started_at.localeCompare(b.started_at)).map((r) => r.status);
  expect(attempts("flaky"), "retries: two attempts failed and the third passed").toEqual(["failed", "failed", "succeeded"]);
  expect(attempts("after-flaky")).toEqual(["succeeded"]);
  expect(attempts("always-fails")).toEqual(["failed"]);
  expect(attempts("runs-anyway"), "continue on failure did not let the next step run").toEqual(["succeeded"]);
  expect(attempts("hard-fail")).toEqual(["failed"]);
  expect(attempts("never-runs"), "a step after a hard failure ran").toEqual([]);
  expect(await runStatus(request, id)).toBe("failed");
  assertNoErrors();
});

test("the name and inventory typed over the sample survive a reload with no other change", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openSample(page);
  await page.locator("#wf-name").fill("Persist me");
  await page.locator("#wf-inventory").fill("production");
  const before = await step(page, "configure").boundingBox();

  await page.reload();
  await expect(page.locator(".wf-node")).toHaveCount(4);
  await expect(page.locator("#wf-name")).toHaveValue("Persist me");
  await expect(page.locator("#wf-inventory")).toHaveValue("production");
  const after = await step(page, "configure").boundingBox();
  expect(Math.abs(after.x - before.x)).toBeLessThan(4);
  expect(Math.abs(after.y - before.y)).toBeLessThan(4);
  assertNoErrors();
});

test("a drawn graph, its name and its inventory all survive a reload", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openEmptyEditor(page);
  await addFirst(page, bashStep("alpha", "echo a"));
  await addAfter(page, "alpha", bashStep("beta", "echo b"));
  await page.locator("#wf-name").fill("Drawn and kept");
  await page.locator("#wf-inventory").fill("inv.ini");
  const before = await snapshot(page);

  await page.reload();
  await expect(page.locator(".wf-node")).toHaveCount(2);
  expect(await snapshot(page)).toEqual(before);
  await expect(page.locator("#wf-name")).toHaveValue("Drawn and kept");
  await expect(page.locator("#wf-inventory")).toHaveValue("inv.ini");
  assertNoErrors();
});

test("every refusal says why and leaves the graph as it was", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openSample(page);
  const original = await snapshot(page);

  // A link that would close a loop, a link to itself, and a link that already exists.
  await dragLink(page, "smoke-test", "provision");
  await expect(page.locator("#status")).toHaveText("That link would create a cycle.");
  expect(await snapshot(page)).toEqual(original);
  await dragLink(page, "provision", "provision");
  expect(await snapshot(page)).toEqual(original);
  await dragLink(page, "provision", "configure");
  expect(await snapshot(page)).toEqual(original);

  // A second step with a name already taken, and a bash step with nothing to run.
  await page.locator("#wf-add").click();
  await fillStep(page, bashStep("provision", "echo again"));
  await expect(page.locator("#wf-step-status")).toHaveText("A step named provision already exists.");
  await expect(page.locator("#wf-step-modal")).toBeVisible();
  await page.locator("#wf-step-name").fill("blank");
  await page.locator("#wf-step-command").fill("");
  await page.locator('#wf-step-form button[type="submit"]').click();
  await expect(page.locator("#wf-step-status")).toHaveText("A bash step needs a command.");
  await page.keyboard.press("Escape");
  await expect(page.locator("#wf-step-modal")).toBeHidden();
  expect(await snapshot(page)).toEqual(original);
  assertNoErrors();
});

test("undo and redo walk a mixed sequence of edits exactly", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openSample(page);
  const states = [await snapshot(page)];

  await addAfter(page, "provision", bashStep("lint", "echo lint"));
  states.push(await snapshot(page));
  await dragLink(page, "lint", "smoke-test");
  states.push(await snapshot(page));
  await step(page, "configure").locator(".wf-node-del").click();
  states.push(await snapshot(page));
  await step(page, "migrate-db").locator(".wf-node-target").click();
  await page.locator("#wf-step-name").fill("migrate");
  await page.locator('#wf-step-form button[type="submit"]').click();
  await expect(page.locator("#wf-step-modal")).toBeHidden();
  states.push(await snapshot(page));
  expect(states[4].names).toContain("migrate");

  for (let i = states.length - 2; i >= 0; i--) {
    await page.keyboard.press("ControlOrMeta+z");
    expect(await snapshot(page), `undo did not return to state ${i}`).toEqual(states[i]);
  }
  for (let i = 1; i < states.length; i++) {
    await page.keyboard.press("ControlOrMeta+Shift+z");
    expect(await snapshot(page), `redo did not return to state ${i}`).toEqual(states[i]);
  }
  assertNoErrors();
});

// mulberry32 is a small seeded generator, so a failing walk can be replayed from its seed.
function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// reaches reports whether a path of links leads from one step to another in the model.
function reaches(edges, from, to) {
  const seen = new Set();
  const stack = [from];
  while (stack.length) {
    const at = stack.pop();
    if (at === to) return true;
    if (seen.has(at)) continue;
    seen.add(at);
    for (const e of edges) if (e.from === at) stack.push(e.to);
  }
  return false;
}

// Each seed is one random walk of editing gestures, checked after every gesture against a model the
// test keeps on its own, and ended by running what the editor exported.
for (const seed of [11, 2024]) {
  test(`a random walk of ${seed} edits keeps the editor, an independent model and the server in agreement`, async ({ page, request }) => {
    test.setTimeout(180_000);
    const assertNoErrors = attachErrorGuards(page);
    const rand = mulberry32(seed);
    const pick = (list) => list[Math.floor(rand() * list.length)];
    const trail = [];

    await openEmptyEditor(page);
    const model = { names: new Set(["n1"]), edges: [] };
    let counter = 1;
    await addFirst(page, bashStep("n1", "echo n1"));

    const expected = () => ({
      names: [...model.names].sort(),
      edges: model.edges.map((e) => dep(e.from, e.to)).sort(),
    });
    const check = async (what) => {
      trail.push(what);
      expect(await snapshot(page), `after ${trail.length} edits, the last being: ${what}\n${trail.join("\n")}`)
        .toEqual(expected());
    };

    for (let i = 0; i < 32; i++) {
      const names = [...model.names];
      const roll = rand();
      if (roll < 0.22 && names.length < 9) {
        const from = pick(names);
        const name = `n${++counter}`;
        await addAfter(page, from, bashStep(name, `echo ${name}`));
        model.names.add(name);
        model.edges.push({ from, to: name });
        await check(`add ${name} after ${from}`);
      } else if (roll < 0.34 && names.length < 9) {
        const name = `n${++counter}`;
        await addFromToolbar(page, bashStep(name, `echo ${name}`));
        model.names.add(name);
        await check(`add ${name} unlinked`);
      } else if (roll < 0.46 && names.length > 1) {
        const name = pick(names);
        await step(page, name).locator(".wf-node-del").click();
        model.names.delete(name);
        model.edges = model.edges.filter((e) => e.from !== name && e.to !== name);
        await check(`delete ${name}`);
      } else if (roll < 0.78 && names.length > 1) {
        const from = pick(names);
        const to = rand() < 0.1 ? from : pick(names);
        await page.locator("#wf-zoom-fit").click();
        await dragLink(page, from, to);
        const duplicate = model.edges.some((e) => e.from === from && e.to === to);
        const loop = from === to || reaches(model.edges, to, from);
        if (!duplicate && !loop) model.edges.push({ from, to });
        await check(`link ${from} to ${to}${duplicate || loop ? " (refused)" : ""}`);
      } else if (roll < 0.9 && model.edges.length) {
        // Only a link drawn left to right is picked: its midpoint is the middle of its box.
        const boxes = Object.fromEntries(await Promise.all(names.map(async (n) => [n, await step(page, n).boundingBox()])));
        const clickable = model.edges.filter((e) => boxes[e.from].x < boxes[e.to].x);
        if (!clickable.length) continue;
        const edge = pick(clickable);
        const hit = page.locator(`.wf-edge-hit[aria-label^="${dep(edge.from, edge.to)}."]`);
        await hit.dispatchEvent("click");
        // A real click on a link takes focus off whichever card the last gesture left it on, and
        // Delete removes a focused card before it removes a selected link.
        await page.evaluate(() => document.activeElement && document.activeElement.blur());
        await page.keyboard.press("Delete");
        model.edges = model.edges.filter((e) => e !== edge);
        await check(`unlink ${edge.from} from ${edge.to}`);
      } else if (names.length > 1) {
        const from = pick(names);
        const taken = rand() < 0.25;
        const to = taken ? pick(names.filter((n) => n !== from)) : `n${++counter}`;
        await step(page, from).locator(".wf-node-target").click();
        await page.locator("#wf-step-name").fill(to);
        await page.locator('#wf-step-form button[type="submit"]').click();
        if (taken) {
          await expect(page.locator("#wf-step-status")).toHaveText(`A step named ${to} already exists.`);
          await page.keyboard.press("Escape");
        } else {
          model.names.delete(from);
          model.names.add(to);
          model.edges = model.edges.map((e) => ({ from: e.from === from ? to : e.from, to: e.to === from ? to : e.to }));
        }
        await expect(page.locator("#wf-step-modal")).toBeHidden();
        await check(`rename ${from} to ${to}${taken ? " (refused)" : ""}`);
      }
    }

    // The export is the pipeline the editor would run: it must say what the model says.
    const [download] = await Promise.all([page.waitForEvent("download"), page.locator("#wf-export-json").click()]);
    const doc = JSON.parse(readFileSync(await download.path(), "utf8"));
    const exported = Object.fromEntries(doc.steps.map((s) => [s.name, (s.depends_on || []).slice().sort()]));
    const want = Object.fromEntries([...model.names].map((n) => [n, model.edges.filter((e) => e.to === n).map((e) => e.from).sort()]));
    expect(exported, `the export disagrees with the model\n${trail.join("\n")}`).toEqual(want);

    // And the server runs exactly that, every step after every step it depends on.
    doc.name = `walk-${seed}-${RUN}`;
    const started = await request.post("/v1/pipelines", { data: doc });
    expect(started.status(), await started.text()).toBe(202);
    const id = (await started.json()).id;
    expect(await waitForStatus(request, id, ["succeeded", "failed"])).toBe("succeeded");
    const rows = await stepRuns(request, id);
    for (const edge of model.edges) endedBefore(rows, edge.from, edge.to);
    expect(new Set(rows.map((r) => r.step_name)), "the server ran a different set of steps than the model drew")
      .toEqual(model.names);
    assertNoErrors();
  });
}

test("Save as template stores the graph the picture shows", async ({ page, request }) => {
  const assertNoErrors = attachErrorGuards(page);
  const name = `template-${RUN}`;
  await openEmptyEditor(page);
  await addFirst(page, bashStep("one", "echo one"));
  await addAfter(page, "one", bashStep("two", "echo two"));
  await page.locator("#wf-name").fill(name);
  await page.locator("#wf-save-template").click();
  await expect(page.locator("#status")).toContainText(`Saved as the workflow template ${name}`);

  const res = await request.get("/v1/templates");
  const list = (await res.json()).templates;
  const saved = list.find((t) => t.name === name);
  expect(saved, `no template named ${name} among ${list.map((t) => t.name).join(", ")}`).toBeTruthy();
  const steps = saved.steps || saved.pipeline || [];
  expect(steps.map((s) => s.name)).toEqual(["one", "two"]);
  expect(steps.find((s) => s.name === "two").depends_on).toEqual(["one"]);
  assertNoErrors();
});

test("every pattern lays out a graph with no dangling link and no loop", async ({ page }) => {
  const assertNoErrors = attachErrorGuards(page);
  await openSample(page);
  await page.locator("#wf-wizard-open").click();
  const count = await page.locator(".wf-pattern").count();
  expect(count).toBeGreaterThanOrEqual(5);
  for (let i = 0; i < count; i++) {
    if (!(await page.locator("#wf-wizard-modal").isVisible())) await page.locator("#wf-wizard-open").click();
    await page.locator("#wf-wizard-tool").selectOption("bash");
    const label = (await page.locator(".wf-pattern").nth(i).innerText()).split("\n")[0];
    await page.locator(".wf-pattern").nth(i).click();
    await expect(page.locator("#wf-wizard-modal")).toBeHidden();
    const { names, edges } = await snapshot(page);
    expect(names.length, `${label} laid out no steps`).toBeGreaterThan(0);
    expect(new Set(names).size, `${label} repeated a step name`).toBe(names.length);
    const pairs = edges.map((e) => /^(?:Dependency|Deny path) link, (.+) into (.+)$/.exec(e)).map((m) => ({ from: m[1], to: m[2] }));
    for (const p of pairs) {
      expect(names, `${label} links ${p.from} into ${p.to}, which is not drawn`).toContain(p.from);
      expect(names).toContain(p.to);
    }
    for (const p of pairs) expect(reaches(pairs.filter((q) => q !== p), p.to, p.from), `${label} has a loop through ${p.from}`).toBe(false);
  }
  assertNoErrors();
});

test("the runs page offers Approve for a step that arrives after it loaded", async ({ page, request }) => {
  const assertNoErrors = attachErrorGuards(page);
  const name = `live-runs-${RUN}`;
  await page.clock.install();
  await page.goto("/ui/runs");
  await expect(page.locator(".approval-step", { hasText: name })).toHaveCount(0);

  const res = await request.post("/v1/pipelines", { data: { name, steps: [{ name: "gate", type: "approval", description: "Live?" }] } });
  expect(res.status()).toBe(202);
  await waitForStatus(request, (await res.json()).id, ["pending_approval"]);
  // The page asks again on a timer. Jump past it rather than waiting it out.
  await page.clock.fastForward(16_000);
  await expect(page.locator(".approval-step", { hasText: name })).toHaveCount(1);
  assertNoErrors();
});

test("a workflow's own page offers Approve when its step arrives while the page is open", async ({ page, request }) => {
  const assertNoErrors = attachErrorGuards(page);
  const name = `live-detail-${RUN}`;
  await page.clock.install();
  const res = await request.post("/v1/pipelines", { data: { name, steps: [
    { name: "prep", tool: "bash", command: "sleep 3" },
    { name: "gate", type: "approval", description: "Live?", depends_on: ["prep"] },
  ] } });
  expect(res.status()).toBe(202);
  const id = (await res.json()).id;
  await page.goto(`/ui/runs/${id}`);
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);

  await waitForStatus(request, id, ["pending_approval"]);
  await page.clock.fastForward(16_000);
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(1);
  assertNoErrors();
});

test.describe("on a phone", () => {
  test.use({ viewport: { width: 500, height: 900 }, hasTouch: true, isMobile: true });

  test("a step can be added and edited by touch, with no sideways scroll", async ({ page }) => {
    const assertNoErrors = attachErrorGuards(page);
    await page.goto("/ui/workflows");
    await expect(page.locator(".wf-node")).toHaveCount(4);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(1);

    await page.locator("#wf-canvas-add").tap();
    const card = await page.locator("#wf-step-modal .modal-card").boundingBox();
    expect(card.x).toBeGreaterThanOrEqual(0);
    expect(card.x + card.width).toBeLessThanOrEqual(500);
    await fillStepByTouch(page, bashStep("phone-step", "echo phone"));
    await expect(step(page, "phone-step")).toBeVisible();
    await expect(step(page, "phone-step").locator(".wf-node-add")).toHaveCSS("opacity", "1");

    await step(page, "phone-step").locator(".wf-node-target").tap();
    await expect(page.locator("#wf-step-title")).toHaveText("Edit step");
    await page.locator("#wf-step-name").fill("phone-edited");
    await page.locator('#wf-step-form button[type="submit"]').tap();
    await expect(step(page, "phone-edited")).toBeVisible();
    assertNoErrors();
  });
});

// fillStepByTouch fills the step dialog and submits it with a tap.
async function fillStepByTouch(page, fields) {
  await expect(page.locator("#wf-step-modal")).toBeVisible();
  await page.locator("#wf-step-name").fill(fields.name);
  await page.locator("#wf-step-tool").selectOption(fields.tool);
  await page.locator("#wf-step-command").fill(fields.command);
  await page.locator('#wf-step-form button[type="submit"]').tap();
  await expect(page.locator("#wf-step-modal")).toBeHidden();
}
