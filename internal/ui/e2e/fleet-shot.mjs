// fleet-shot.mjs remakes the fleet health picture, site/assets/screenshot-fleet.png, from the local
// seeded demo, so the asset can be rebuilt from a checkout rather than taken by hand. The picture
// appears in the home page gallery and in the README.
//
// Usage, from this directory with the demo running (npm run build:bin, then
// ./.bin/switchtender demo --addr 127.0.0.1:18777):
//
//	node fleet-shot.mjs [demo URL] [output path]
//
// The shot is fleet health in dark scheme at a 1200 px viewport, wide enough that the table is not
// shown as stacked cards, and device scale 2. The Last run column is hidden so its relative times do
// not date the image, and the clip runs from the page heading to the table's right edge, leaving out
// the sidebar and the demo banner. No test checks its size, so after a retake set the gallery img's
// width and height attributes to half the saved pixel size and bump its ?v= query.
import { chromium } from "@playwright/test";
import { fileURLToPath } from "node:url";

const demo = process.argv[2] || "http://127.0.0.1:18777";
const out = process.argv[3] ||
  fileURLToPath(new URL("../../../site/assets/screenshot-fleet.png", import.meta.url));

const browser = await chromium.launch(
  process.env.ST_E2E_CHANNEL ? { channel: process.env.ST_E2E_CHANNEL } : {});
try {
  const page = await browser.newPage({
    viewport: { width: 1200, height: 1000 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
  });
  await page.goto(`${demo}/ui/fleet`, { waitUntil: "networkidle" });
  await page.locator("table tbody tr").first().waitFor();
  // Let the sparklines and badges finish their entry transitions before the capture.
  await page.waitForTimeout(1500);
  // Hide the Last run column, whose relative times would date the image, then clip from the page
  // heading to the table's right edge, wide enough to keep the export buttons whole.
  const clip = await page.evaluate(() => {
    const heading = [...document.querySelectorAll("h1, h2")]
      .find((e) => /Fleet health/.test(e.textContent));
    const table = document.querySelector("table");
    const ths = [...table.querySelectorAll("thead th")];
    const lastRun = ths.findIndex((th) => /last run/i.test(th.textContent));
    if (!heading || lastRun < 0) {
      return null;
    }
    for (const row of table.querySelectorAll("tr")) {
      const cell = row.children[lastRun];
      if (cell) {
        cell.style.display = "none";
      }
    }
    const h = heading.getBoundingClientRect();
    const t = table.getBoundingClientRect();
    const right = Math.max(t.right,
      ...[...document.querySelectorAll("main button, main .btn")].map((b) => b.getBoundingClientRect().right));
    const x = t.left - 12;
    const y = h.top + scrollY - 16;
    return { x, y, width: right + 12 - x, height: t.bottom + scrollY + 12 - y };
  });
  if (!clip) {
    throw new Error("fleet health heading or Last run column not found");
  }
  await page.screenshot({ path: out, clip });
  console.log(`wrote ${out}: ${Math.round(clip.width)} x ${Math.round(clip.height)} CSS px`);
} finally {
  await browser.close();
}
