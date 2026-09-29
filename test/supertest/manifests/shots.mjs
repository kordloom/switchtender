// Screenshots of the deployed UI, taken through the same port-forward the assertions used. The
// supertest writes this file to its working directory and runs it with node; playwright-core is
// borrowed from the repository's own e2e suite rather than installed twice.
//
//   node shots.mjs <repoRoot> <apiBase> <outDir> [runID]
import { createRequire } from "node:module";
import { join } from "node:path";
import { mkdirSync } from "node:fs";

const [repo, api, out, runID] = process.argv.slice(2);
if (!repo || !api || !out) {
  console.error("usage: node shots.mjs <repoRoot> <apiBase> <outDir> [runID]");
  process.exit(1);
}
const require = createRequire(join(repo, "internal/ui/e2e/package.json"));
const { chromium } = require("playwright-core");

mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });

const pages = [
  ["runs", "/ui/runs"],
  ["estate", "/ui/estate"],
  ["policies", "/ui/policies"],
];
if (runID) pages.push(["governed-run", "/ui/runs/" + runID]);

for (const [name, path] of pages) {
  await page.goto(api + path, { waitUntil: "domcontentloaded" });
  // The pages fetch their data after load; a beat keeps empty shells out of the evidence.
  await page.waitForTimeout(1500);
  await page.screenshot({ path: join(out, name + ".png"), fullPage: true });
  console.log("shot", name);
}
await browser.close();
