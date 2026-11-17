// hero-shot.mjs remakes the home page hero image, site/assets/screenshot-held.png, from the local
// seeded demo, so the asset can be rebuilt from a checkout rather than taken by hand.
//
// Usage, from this directory with the demo running (npm run build:bin, then
// ./.bin/switchtender demo --addr 127.0.0.1:18777):
//
//	node hero-shot.mjs [demo URL] [output path]
//
// The shot is the held agent requests page, the page the hero button opens, in dark scheme at a
// 680 px viewport and device scale 2, clipped from the page heading to the bottom of the first held
// run. It hides the top bar, the demo banner, the back link, the closing read-only note, and each
// card's time in its state, so the image does not date. At 680 px each card keeps its labels beside
// their answers and the clip is about 680 x 434 CSS px; the home page's img width and height
// attributes are half the saved pixel size, and cmd/sitegen's hero test fails if they disagree.
import { chromium } from "@playwright/test";
import { fileURLToPath } from "node:url";

const demo = process.argv[2] || "http://127.0.0.1:18777";
const out = process.argv[3] ||
  fileURLToPath(new URL("../../../site/assets/screenshot-held.png", import.meta.url));

const browser = await chromium.launch(
  process.env.ST_E2E_CHANNEL ? { channel: process.env.ST_E2E_CHANNEL } : {});
try {
  const page = await browser.newPage({
    viewport: { width: 680, height: 1100 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
  });
  await page.goto(`${demo}/ui/held?who=agent`, { waitUntil: "networkidle" });
  await page.locator("#held-list .attn-item").first().waitFor();
  // Let the cards finish their entry transitions before the capture.
  await page.waitForTimeout(1500);
  const clip = await page.evaluate(() => {
    const hidden = ".ro-banner, main .back, .attn-since, #held-decide, #held-next";
    for (const el of document.querySelectorAll(hidden)) {
      el.style.display = "none";
    }
    // The top bar keeps its space so the clip has room above the heading, but its edge is not drawn.
    document.querySelector("header.topbar").style.visibility = "hidden";
    const heading = document.querySelector("main h1");
    const first = document.querySelector("#held-list .attn-item");
    if (!heading || !first || !/A person decides/.test(heading.textContent)) {
      return null;
    }
    const m = document.querySelector("main").getBoundingClientRect();
    const y = heading.getBoundingClientRect().top + scrollY - 28;
    const height = first.getBoundingClientRect().bottom + scrollY + 8 - y;
    return { x: m.left, y, width: m.width, height };
  });
  if (!clip) {
    throw new Error("held agent requests heading or first held run not found");
  }
  await page.screenshot({ path: out, clip });
  console.log(`wrote ${out}: ${Math.round(clip.width)} x ${Math.round(clip.height)} CSS px`);
} finally {
  await browser.close();
}
