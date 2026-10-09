import { test, expect } from "@playwright/test";

// The browser assessment, in a real engine. The command's report and the page's are compared byte
// for byte by scripts/assess-conformance.cjs. This covers what only a browser shows: that the page
// reads a dropped file through its worker at all, that an export saved as UTF-16 reads the same,
// and that the page keeps its promise that nothing leaves once it has loaded, which a web font
// fetched to draw a name from the export would break.

// EXPORT is an estate whose template names reach beyond Latin, two of them destructive so their
// names are printed in the report, where a web font would fetch a subset to draw them.
const EXPORT = JSON.stringify({
  projects: [{ name: "infra", scm_type: "git", scm_url: "https://example.invalid/i.git" }],
  job_templates: [
    { name: "Wdrożenie łącza", playbook: "deploy.yml", project: "infra" },
    { name: "Уничтожение данных", playbook: "wipe.yml", project: "infra",
      extra_vars: "cmd: wipefs -a /dev/sdb" },
    { name: "Usuń schemat", playbook: "db.yml", project: "infra",
      extra_vars: "sql: DROP SCHEMA reporting CASCADE" },
  ],
});

// WEB_FONTS are the families the site loads from a font host.
const WEB_FONTS = /Inter|Space Grotesk|JetBrains Mono/;

// utf16 saves text the way Windows PowerShell writes a file by default.
function utf16(text) {
  return Buffer.concat([Buffer.from([0xff, 0xfe]), Buffer.from(text, "utf16le")]);
}

// drop hands the page a file the way a drag from the desktop does, and waits for its answer.
async function drop(page, bytes, name) {
  await page.evaluate(({ bytes, name }) => {
    const dt = new DataTransfer();
    dt.items.add(new File([new Uint8Array(bytes)], name, { type: "application/json" }));
    document.getElementById("drop").dispatchEvent(
      new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true }));
  }, { bytes: Array.from(bytes), name });
  await expect(page.locator("#drop-main")).toHaveText(/^(Assessed |Drop another)/,
    { timeout: 60_000 });
  return {
    drop: await page.locator("#drop-main").textContent(),
    headline: await page.locator("#headline").textContent(),
    report: await page.locator("#report-text").textContent(),
  };
}

test("reads a dropped export in a worker and fetches nothing once loaded", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  let loaded = false;
  const after = [];
  page.on("request", (r) => { if (loaded) { after.push(r.url()); } });
  await page.goto("/assess/");
  await expect(page.locator("#drop-main")).toHaveText(/^Drop an export here/,
    { timeout: 60_000 });
  loaded = true;

  // Test 0: the export is read into the report and the module's own headline, by the worker, since
  // the page itself never holds the reader.
  const plain = await drop(page, Buffer.from(EXPORT, "utf8"), "экспорт-Überführung-ł.json");
  expect(plain.drop).toContain("экспорт-Überführung-ł.json");
  expect(plain.headline).toBe("2 of your 3 templates can do something nobody can undo, and " +
    "today they run whenever somebody presses the button. One approval policy holds them until " +
    "a second person agrees.");
  expect(plain.report).toContain("Уничтожение данных");
  expect(plain.report).toContain("Usuń schemat");
  expect(await page.evaluate(() => typeof window.switchtenderAssess)).toBe("undefined");
  // The figures carry the count the grades rest on: every template here runs a playbook nothing
  // has fetched, and that number sits beside the grades rather than only in the report below.
  const figures = await page.locator("#figures .figure").evaluateAll((els) =>
    els.map((el) => el.textContent));
  expect(figures).toContain("3playbooks not read yet");

  // Test 1: the same export saved as UTF-16 reads the same.
  const wide = await drop(page, utf16(EXPORT), "export-utf16.json");
  expect(wide.report).toBe(plain.report);

  // Test 2: text from the file is set in fonts already on the machine.
  const fontOf = (selector) =>
    page.locator(selector).evaluate((el) => getComputedStyle(el).fontFamily);
  expect(await fontOf("#report-text")).not.toMatch(WEB_FONTS);
  expect(await fontOf("#drop-main .file-name")).not.toMatch(WEB_FONTS);

  // Test 3: nothing was requested once the page had loaded.
  await page.waitForTimeout(1_000);
  expect(after).toEqual([]);
  expect(errors).toEqual([]);
});
