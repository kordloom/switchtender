// Screenshots of the deployed UI, taken through the same port-forward the assertions used. The
// supertest writes this file to its working directory and runs it with node; playwright-core is
// borrowed from the repository's own e2e suite rather than installed twice.
//
// The browser signs in before the first capture, through the token form on the sign-in page, with
// the token in SUPERTEST_SHOTS_TOKEN. The token comes from the environment and not from the
// arguments, which every account on the machine can read in the process list, and it is never
// printed. A page that shows the sign-in form after that ends the script nonzero, naming the page:
// a signed-out browser is sent to that form from every page, so each capture would be the form
// under another page's name.
//
//   SUPERTEST_SHOTS_TOKEN=<token> node shots.mjs <repoRoot> <apiBase> <outDir> [runID]
import { createRequire } from "node:module";
import { join } from "node:path";
import { mkdirSync } from "node:fs";

// TOKEN_ENV names the environment variable that carries the token the browser signs in with.
const TOKEN_ENV = "SUPERTEST_SHOTS_TOKEN";

// SIGN_IN_WAIT bounds how long the sign-in may take to open the interface, in milliseconds.
const SIGN_IN_WAIT = 30_000;

const [repo, api, out, runID] = process.argv.slice(2);
if (!repo || !api || !out) {
  console.error(`usage: ${TOKEN_ENV}=<token> node shots.mjs <repoRoot> <apiBase> <outDir> [runID]`);
  process.exit(1);
}
const token = process.env[TOKEN_ENV] || "";
if (!token) {
  console.error(`${TOKEN_ENV} is not set, so the browser cannot sign in before it captures`);
  process.exit(1);
}
// The browser and its helpers are child processes, and none of them needs the token.
delete process.env[TOKEN_ENV];

const require = createRequire(join(repo, "internal/ui/e2e/package.json"));
const { chromium } = require("playwright-core");

// redact removes the token from text bound for the log, so no failure can print it.
function redact(text) {
  return String(text).split(token).join("<token>");
}

// showsSignIn reports whether the page on screen is the sign-in form.
async function showsSignIn(page) {
  const form = page.locator('body[data-page="login"], #account-form, #login-form');
  return (await form.count()) > 0;
}

// signIn signs the browser in the way a person holding a token does: through the token form on the
// sign-in page, which keeps the session in the browser and then opens the interface.
async function signIn(page) {
  await page.goto(api + "/ui/login", { waitUntil: "load" });
  // The token form is folded behind its summary on an install that has accounts.
  const folded = page.locator("details.alt-auth");
  if (!(await folded.evaluate((el) => el.open))) await folded.locator("summary").click();
  await page.locator("#token-input").fill(token);
  await page.locator('#login-form button[type="submit"]').click();
  try {
    await page.waitForURL((url) => url.pathname === "/ui/", { timeout: SIGN_IN_WAIT });
  } catch {
    const said = (await page.locator("#status").textContent().catch(() => "")) || "";
    throw new Error("the sign-in did not open the interface: " +
      (said.trim() || "the sign-in page gave no reason"));
  }
}

mkdirSync(out, { recursive: true });
let browser;
let failed = false;
try {
  browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await signIn(page);

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
    if (await showsSignIn(page)) {
      throw new Error(`the ${name} page at ${path} shows the sign-in form, so it was not captured`);
    }
    await page.screenshot({ path: join(out, name + ".png"), fullPage: true });
    console.log("shot", name);
  }
} catch (err) {
  failed = true;
  console.error(redact(err && err.message ? err.message : err));
} finally {
  if (browser) await browser.close();
}
if (failed) process.exit(1);
