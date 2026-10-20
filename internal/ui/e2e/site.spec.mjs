import { test, expect } from "@playwright/test";

// The home page hero, measured in a real engine against the site as it deploys. At every
// two-column width the fleet health screenshot starts level with the headline, sits beside the
// copy, and renders large enough that its table text reads. At one-column widths nothing widens the
// page and the screenshot follows the copy. The screenshot used to be centered against the whole
// text column, 300 px below the headline, and shown at a third of its size.

// TWO_COLUMN are the desktop viewports where the shot sits beside the copy.
const TWO_COLUMN = [[1440, 900], [1280, 800], [1024, 768]];

// ONE_COLUMN are the viewports where the hero stacks.
const ONE_COLUMN = [[900, 1000], [390, 844]];

// open loads the home page at a viewport with the scroll reveal settled, since the reveal's
// transform would move every box mid-measurement, and waits for the hero image to decode.
async function open(page, [width, height]) {
  await page.setViewportSize({ width, height });
  await page.goto("/");
  await page.addStyleTag({
    content: ".reveal{opacity:1!important;transform:none!important;transition:none!important}",
  });
  await page.locator(".hero-shot img").evaluate((img) => img.decode());
}

// measure returns the hero's boxes in page coordinates and the image's rendered scale against its
// capture size in CSS pixels, which is its width attribute. cmd/sitegen's hero test holds that
// attribute to half the image's pixel width, since it is captured at device scale 2, and an image
// without the attribute has no stated capture size and fails the scale check.
function measure(page) {
  return page.evaluate(() => {
    const box = (sel) => {
      const r = document.querySelector(sel).getBoundingClientRect();
      return { top: r.top + scrollY, bottom: r.bottom + scrollY, left: r.left, right: r.right };
    };
    const img = document.querySelector(".hero-shot img");
    const captured = Number(img.getAttribute("width")) || 0;
    return {
      h1: box(".hero-copy h1"),
      copy: box(".hero-copy"),
      shot: box(".hero-shot .window"),
      scale: captured > 0 ? img.getBoundingClientRect().width / captured : 0,
      docWidth: document.documentElement.scrollWidth,
      viewWidth: window.innerWidth,
    };
  });
}

for (const size of TWO_COLUMN) {
  test(`at ${size[0]} the screenshot is level with the headline and readable`, async ({ page }) => {
    await open(page, size);
    const m = await measure(page);
    // The window's tilt moves its painted corner a few pixels, so level means within 8. Each check
    // is soft so one failure still reports the others.
    expect.soft(Math.abs(m.shot.top - m.h1.top), `shot top ${m.shot.top}, h1 top ${m.h1.top}`)
      .toBeLessThanOrEqual(8);
    expect.soft(m.shot.left, "the shot sits beside the copy").toBeGreaterThan(m.copy.right);
    expect.soft(m.scale, "rendered width over capture width").toBeGreaterThanOrEqual(0.6);
  });
}

for (const size of ONE_COLUMN) {
  test(`at ${size[0]} the hero stacks without widening the page`, async ({ page }) => {
    await open(page, size);
    const m = await measure(page);
    expect(m.docWidth, "document width").toBeLessThanOrEqual(m.viewWidth);
    expect(m.shot.top, "the shot follows the copy").toBeGreaterThanOrEqual(m.copy.bottom);
  });
}
