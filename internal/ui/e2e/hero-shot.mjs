// hero-shot.mjs remakes the home page hero image, site/assets/screenshot-fleet.png, from the local
// seeded demo, so the asset can be rebuilt from a checkout rather than taken by hand.
//
// Usage, from this directory with the demo running (npm run build:bin, then
// ./.bin/switchtender demo --addr 127.0.0.1:18777):
//
//	node hero-shot.mjs [demo URL] [output path]
//
// The shot is fleet health in dark scheme at a 760 px viewport and device scale 2, clipped from the
// page heading to the right edge of the Last outcome column. That leaves out the sidebar, the demo
// banner, and the Last run column's relative times, so the image does not date. The clip is about
// 692 x 517 CSS px; the home page's img width and height attributes are half the saved pixel size,
// and cmd/sitegen's hero test fails if they disagree.
import { chromium } from "@playwright/test";
import { fileURLToPath } from "node:url";

const demo = process.argv[2] || "http://127.0.0.1:18777";
const out = process.argv[3] ||
  fileURLToPath(new URL("../../../site/assets/screenshot-fleet.png", import.meta.url));

const browser = await chromium.launch(
  process.env.ST_E2E_CHANNEL ? { channel: process.env.ST_E2E_CHANNEL } : {});
try {
  const page = await browser.newPage({
    viewport: { width: 760, height: 1000 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
  });
  await page.goto(`${demo}/ui/fleet`, { waitUntil: "networkidle" });
  await page.locator("table tbody tr").first().waitFor();
  // Let the sparklines and badges finish their entry transitions before the capture.
  await page.waitForTimeout(1500);
  const clip = await page.evaluate(() => {
    const heading = [...document.querySelectorAll("h1, h2")]
      .find((e) => /Fleet health/.test(e.textContent));
    const table = document.querySelector("table");
    const last = [...table.querySelectorAll("th")]
      .find((th) => /last outcome/i.test(th.textContent));
    if (!heading || !last) {
      return null;
    }
    const h = heading.getBoundingClientRect();
    const t = table.getBoundingClientRect();
    const x = t.left - 12;
    const y = h.top + scrollY - 16;
    return { x, y, width: last.getBoundingClientRect().right - x, height: t.bottom + scrollY + 12 - y };
  });
  if (!clip) {
    throw new Error("fleet health heading or Last outcome column not found");
  }
  await page.screenshot({ path: out, clip });
  console.log(`wrote ${out}: ${Math.round(clip.width)} x ${Math.round(clip.height)} CSS px`);
} finally {
  await browser.close();
}
