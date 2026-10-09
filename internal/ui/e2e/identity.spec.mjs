import { test, expect, request as apiRequest } from "@playwright/test";

// These drive the stored identity against a writable serve instance of their own, where accounts
// are created, demoted, promoted, renamed, and deleted through the real API while a browser holds a
// session for the account being changed. Every page asks the server who the session is before
// drawing from the cache, and asks again after an action on accounts, so the navigation, the badge,
// the Users page, and the sign-in page follow the account's current role and name, and a deleted
// account's session is forgotten.
//
// The instance is this suite's alone. The first account created turns an open install into one that
// authenticates, which would refuse every other suite that drives the shared serve instance open.

// PASSWORD is every account's password. The instance is created for this run and thrown away.
const PASSWORD = "e2e-identity-password-not-a-secret";

// ADMIN_PAGES are the navigation labels only an admin is offered.
const ADMIN_PAGES = ["Migrate", "Credentials", "Users", "Audit", "Policies", "Doctor"];

// The tests change the same three accounts in turn, so they run in order.
test.describe.configure({ mode: "serial" });

// api is a client on the identity server holding the second administrator's session, the one that
// changes the accounts the browser is signed in as.
let api;

// ids maps each username to the account id the server assigned it.
const ids = {};

test.beforeAll(async ({}, testInfo) => {
  const baseURL = testInfo.project.use.baseURL;
  const open = await apiRequest.newContext({ baseURL });
  // The first account turns the open install into one that authenticates, so the administrator who
  // will change the others is created first and signs in before anyone else can be created.
  const created = await open.post("/v1/users", {
    data: { username: "admin2", password: PASSWORD, role: "admin" },
  });
  expect(created.status(), await created.text()).toBe(201);
  ids.admin2 = (await created.json()).id;
  // The gate re-reads the account table at most every few seconds, so the install keeps answering
  // open for a moment after its first account exists. Everything below needs it enforcing.
  await expect.poll(async () => (await open.get("/v1/auth/me")).status(), {
    message: "the install never started enforcing after its first account was created",
    timeout: 20_000,
  }).toBe(401);
  const login = await open.post("/v1/auth/login", { data: { username: "admin2", password: PASSWORD } });
  expect(login.status(), await login.text()).toBe(200);
  const { token } = await login.json();
  await open.dispose();
  api = await apiRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
  for (const [username, role] of [["admin1", "admin"], ["viewer1", "viewer"]]) {
    const res = await api.post("/v1/users", { data: { username, password: PASSWORD, role } });
    expect(res.status(), await res.text()).toBe(201);
    ids[username] = (await res.json()).id;
  }
});

test.afterAll(async () => {
  if (api) await api.dispose();
});

// watch records page errors, console errors, and every refused API answer, so a test can assert the
// page did not quietly meet a refusal it was supposed to know better than to ask for.
function watch(page) {
  const errors = [];
  const refused = [];
  page.on("pageerror", (err) => errors.push(`pageerror: ${err.message}`));
  page.on("console", (msg) => {
    if (msg.type() === "error") errors.push(`console.error: ${msg.text()}`);
  });
  page.on("response", (res) => {
    if (!res.url().includes("/v1/") || res.status() < 400) return;
    refused.push(`${res.request().method()} ${new URL(res.url()).pathname} -> ${res.status()}`);
  });
  return { errors, refused };
}

// signIn signs the browser in through the real form and lands on the overview.
async function signIn(page, username) {
  await page.goto("/ui/login");
  await page.locator("#username-input").fill(username);
  await page.locator("#password-input").fill(PASSWORD);
  await page.locator('#account-form button[type="submit"]').click();
  await expect(page).toHaveURL(/\/ui\/$/);
}

// navLabels lists the page labels the docked sidebar offers.
function navLabels(page) {
  return page.locator("aside.side .nav-item .nav-label").allTextContents();
}

// setAccount changes an account's role, and optionally its username, through the API as the second
// administrator.
async function setAccount(username, role, newName) {
  const res = await api.put(`/v1/users/${ids[username]}`, {
    data: { username: newName || username, role },
  });
  expect(res.status(), await res.text()).toBe(200);
}

// stored reads one key of the page's local storage.
function stored(page, key) {
  return page.evaluate((k) => localStorage.getItem(k), key);
}

test("an admin demoted by another admin loses the admin pages on the next load", async ({ page }) => {
  const seen = watch(page);
  await signIn(page, "admin1");
  await page.goto("/ui/users");
  await expect(page.locator("#users tr")).toHaveCount(3);
  await expect(page.locator("#user-open")).toBeVisible();
  await expect(page.locator("aside.side .account-role")).toHaveText("admin");
  expect(await navLabels(page)).toEqual(expect.arrayContaining(ADMIN_PAGES));

  await setAccount("admin1", "operator");

  // The page draws what the server says the session is now: the sentence written for a session
  // below admin, no admin controls, the operator badge, and no admin navigation. Nothing admin
  // only is asked for, so nothing is refused.
  seen.refused.length = 0;
  await page.reload();
  await expect(page.locator("#status"))
    .toContainText("Users and API tokens are managed by admins. You are signed in as an operator");
  await expect(page.locator("#user-open")).toBeHidden();
  await expect(page.locator("#token-open")).toHaveCount(0);
  await expect(page.locator("aside.side .account-role")).toHaveText("operator");
  const labels = await navLabels(page);
  for (const label of ADMIN_PAGES) {
    expect(labels, `the demoted admin is still offered ${label}`).not.toContain(label);
  }
  expect(await stored(page, "st_role")).toBe("operator");
  expect(seen.refused, "the page asked for what the server refuses this role").toEqual([]);
  expect(seen.errors).toEqual([]);
});

test("a viewer promoted to admin gains the admin pages on the next load", async ({ page }) => {
  const seen = watch(page);
  await signIn(page, "viewer1");
  await page.goto("/ui/users");
  await expect(page.locator("#status")).toContainText("You are signed in as a viewer");
  await expect(page.locator("aside.side .account-role")).toHaveText("viewer");
  expect(await navLabels(page)).not.toContain("Users");

  await setAccount("viewer1", "admin");

  seen.refused.length = 0;
  await page.reload();
  await expect(page.locator("#users tr")).toHaveCount(3);
  await expect(page.locator("#user-open")).toBeVisible();
  await expect(page.locator("#token-open")).toBeVisible();
  await expect(page.locator("aside.side .account-role")).toHaveText("admin");
  expect(await navLabels(page)).toEqual(expect.arrayContaining(ADMIN_PAGES));
  await expect(page.locator("body")).not.toContainText("managed by admins");
  expect(seen.refused).toEqual([]);
  expect(seen.errors).toEqual([]);
});

test("an admin who demotes their own account is redrawn as the operator they became", async ({ page }) => {
  // No second browser and no API call: the Edit dialog on the account's own row. This is the way
  // somebody trying the roles out reaches the stale cache first.
  const seen = watch(page);
  await signIn(page, "viewer1");
  await page.goto("/ui/users");
  await expect(page.locator("#users tr")).toHaveCount(3);
  const own = page.locator("#users tr", { hasText: "viewer1" });
  await own.getByRole("button", { name: "Edit" }).click();
  await expect(page.locator("#user-modal")).toBeVisible();
  await page.locator("#user-role").selectOption("operator");

  seen.refused.length = 0;
  await page.locator('#user-form button[type="submit"]').click();
  await expect(page.locator("#status")).toContainText("You are signed in as an operator");
  await expect(page.locator("#user-open")).toBeHidden();
  await expect(page.locator("aside.side .account-role")).toHaveText("operator");
  expect(await navLabels(page)).not.toContain("Users");
  expect(seen.refused, "the page read the list it had just lost the right to").toEqual([]);
  expect(seen.errors).toEqual([]);
});

test("an account renamed by another admin shows its new name on the next load", async ({ page }) => {
  const seen = watch(page);
  await signIn(page, "admin1");
  await expect(page.locator("aside.side .account-name")).toHaveText("admin1");

  await setAccount("admin1", "operator", "admin1-renamed");

  await page.reload();
  await expect(page.locator("aside.side .account-name")).toHaveText("admin1-renamed");
  // The sign-in page names the stored session the same way.
  await page.goto("/ui/login");
  await expect(page.locator("#signed-in-return")).toBeVisible();
  await expect(page.locator("#signed-in-name")).toHaveText("as admin1-renamed");
  expect(seen.errors).toEqual([]);
});

test("a deleted account's session is forgotten rather than offered on the sign-in page", async ({ page }) => {
  const seen = watch(page);
  await signIn(page, "viewer1");
  await expect(page.locator("aside.side .account-name")).toHaveText("viewer1");

  const res = await api.delete(`/v1/users/${ids.viewer1}`);
  expect(res.status(), await res.text()).toBe(200);

  // The next page asks the server who the session is, is told nobody, forgets it, and walks to
  // sign in, which then has no dead session to offer. The one refusal is that single question,
  // asked before the page asks for any of its data.
  seen.refused.length = 0;
  await page.goto("/ui/runs");
  await expect(page).toHaveURL(/\/ui\/login$/);
  await expect(page.locator("#signed-in-return")).toBeHidden();
  expect(await stored(page, "st_token")).toBeNull();
  expect(await stored(page, "st_role")).toBeNull();
  expect(await stored(page, "st_user")).toBeNull();
  expect(seen.refused).toEqual(["GET /v1/auth/me -> 401"]);
  expect(seen.errors.filter((line) => !/401/.test(line))).toEqual([]);
});

test("an admin who deletes their own account lands on sign in with the session forgotten", async ({ page }) => {
  // The Delete button on the account's own row, clicked by that account. The second administrator
  // stays, so the server allows the delete.
  const created = await api.post("/v1/users", { data: { username: "admin3", password: PASSWORD, role: "admin" } });
  expect(created.status(), await created.text()).toBe(201);
  const seen = watch(page);
  await signIn(page, "admin3");
  await page.goto("/ui/users");
  const own = page.locator("#users tr", { hasText: "admin3" });
  await expect(own).toHaveCount(1);
  page.once("dialog", (dialog) => dialog.accept());

  seen.refused.length = 0;
  await own.getByRole("button", { name: "Delete" }).click();
  await expect(page).toHaveURL(/\/ui\/login$/);
  await expect(page.locator("#signed-in-return")).toBeHidden();
  expect(await stored(page, "st_token")).toBeNull();
  expect(await stored(page, "st_role")).toBeNull();
  expect(await stored(page, "st_user")).toBeNull();
  expect(await stored(page, "st_account")).toBeNull();
  expect(seen.refused).toEqual(["GET /v1/auth/me -> 401"]);
  expect(seen.errors.filter((line) => !/401/.test(line))).toEqual([]);
});

test("the credentials page asks for nothing the server said is off", async ({ page }) => {
  // This instance serves tokens and credential types and runs without federation. The page loads
  // the credential types and the federation keys in the same step of its boot, so by the time the
  // credential types section is drawn, any request for the keys has already been sent.
  const seen = watch(page);
  await signIn(page, "admin2");
  const asked = [];
  page.on("request", (req) => {
    if (req.url().includes("/v1/")) asked.push(new URL(req.url()).pathname);
  });
  const types = page.waitForResponse((res) => new URL(res.url()).pathname === "/v1/credential-types");
  await page.goto("/ui/credentials");
  expect((await types).status()).toBe(200);
  await expect(page.locator("#ctype-section")).toBeVisible();
  expect(asked).toContain("/v1/credential-types");
  expect(asked).not.toContain("/v1/federation/keys");
  await expect(page.locator("#fedkey-section")).toBeHidden();
  expect(seen.refused).toEqual([]);
  expect(seen.errors).toEqual([]);
});
