// Tests for linked forge accounts: the page lists each forge the server can link with the account
// linked there by its numeric id and never a login, starts a link by sending the browser to the
// forge, says what the callback reported, unlinks on request, and the run page says when a
// decision came from a pull request comment.
import { test } from "node:test";
import assert from "node:assert/strict";

import { ALL_PARTS, loadParts, sandboxOf } from "./loader.mjs";
import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// GHE is a GitHub Enterprise Server forge as the list route answers it.
const GHE = {
	provider: "github", api_url: "https://ghe.example.com/api/v3",
	web_url: "https://ghe.example.com", host: "ghe.example.com",
};

// GITLAB is the public GitLab service as the list route answers it.
const GITLAB = {
	provider: "gitlab", api_url: "https://gitlab.com/api/v4", web_url: "https://gitlab.com",
	host: "gitlab.com",
};

// openLinks mounts the linked accounts page with the list route answering list and any extra
// routes, and loads it.
async function openLinks(list, extra, hash) {
	const page = loadPage("links", {
		routes: (extra || []).concat([[/^\/v1\/me\/forge-links$/, reply(list)]]),
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "viewer");
	if (hash) win.location.hash = hash;
	await page.app.loadForgeLinks();
	await page.clock.flush();
	return page;
}

test("each forge is listed with its linked account by numeric id and its action", async () => {
	const page = await openLinks({
		forges: [GHE, GITLAB],
		links: [{ id: "fl_1", user_id: "usr_a", provider: "github", api_url: GHE.api_url,
			forge_user_id: 4242, created_at: "2026-10-05T12:00:00Z" }],
	});
	const rows = page.document.querySelectorAll("#links tr");
	assert.equal(rows.length, 2, "one row per forge");
	assert.equal(page.document.getElementById("links-table").hidden, false);
	const first = rows[0].textContent;
	assert.ok(first.includes("GitHub") && first.includes("ghe.example.com"), first);
	assert.ok(first.includes("Account id 4242"), "the account is not named by its id: " + first);
	assert.equal(rows[0].querySelector("button[data-action]").textContent, "Unlink");
	const second = rows[1].textContent;
	assert.ok(second.includes("GitLab") && second.includes("Not linked"), second);
	assert.equal(rows[1].querySelector("button[data-action]").textContent, "Link GitLab account");
});

test("linking posts the forge and sends the browser to the forge's sign-in", async () => {
	const authorize = "https://gitlab.com/oauth/authorize?client_id=x&state=s";
	const page = await openLinks({ forges: [GITLAB], links: [] }, [
		[(r) => r.method === "POST" && r.path === "/v1/me/forge-links",
			reply({ authorize_url: authorize })],
	]);
	fire(page.document.querySelector("#links button[data-action]"), "click");
	await page.clock.flush();
	const posts = page.net.calls.filter((c) => c.method === "POST");
	assert.equal(posts.length, 1, "the link was not started");
	assert.deepEqual(JSON.parse(posts[0].body), { provider: "gitlab", api_url: GITLAB.api_url });
	assert.deepEqual(sandboxOf(page.app).location.navigations, [authorize],
		"the browser was not sent to the forge");
});

test("a refused start says why and leaves the page where it is", async () => {
	const page = await openLinks({ forges: [GITLAB], links: [] }, [
		[(r) => r.method === "POST", reply({ error: "linking needs the server's public address" },
			{ status: 409 })],
	]);
	const button = page.document.querySelector("#links button[data-action]");
	fire(button, "click");
	await page.clock.flush();
	assert.match(page.document.getElementById("links-notice").textContent, /public address/);
	assert.equal(button.disabled, false, "the button stays disabled after a refusal");
	assert.deepEqual(sandboxOf(page.app).location.navigations, []);
});

test("the callback's answer is shown once and stripped from the address", async () => {
	const tests = [
		// Test 0: A link that landed.
		{ Hash: "#linked=github&host=ghe.example.com",
			Want: "Linked your GitHub account on ghe.example.com." },
		// Test 1: A link the server refused says why.
		{ Hash: "#error=this+forge+account+is+a+bot",
			Want: "Nothing was linked: this forge account is a bot" },
	];
	for (const [i, tc] of tests.entries()) {
		const page = loadPage("links", {
			routes: [[/^\/v1\/me\/forge-links$/, reply({ forges: [GHE], links: [] })]],
		});
		const win = sandboxOf(page.app);
		const replaced = [];
		win.history.replaceState = (_s, _t, url) => replaced.push(url);
		win.location.hash = tc.Hash;
		await page.app.loadForgeLinks();
		await page.clock.flush();
		const notice = page.document.getElementById("links-notice");
		assert.equal(notice.textContent, tc.Want, "test " + i);
		assert.equal(notice.hidden, false, "test " + i);
		assert.equal(replaced.length, 1, "test " + i + ": the fragment was not stripped");
	}
});

test("a server with no forge set up says how to add one", async () => {
	const page = await openLinks({ forges: [], links: [] });
	const status = page.document.getElementById("status").textContent;
	assert.match(status, /--forge-oauth/);
	assert.equal(page.document.getElementById("links-table").hidden, true);
});

test("unlinking deletes the link and draws the page again", async () => {
	let listed = 0;
	const page = loadPage("links", {
		routes: [
			[(r) => r.method === "DELETE" && r.path === "/v1/me/forge-links/fl_1", reply({})],
			[/^\/v1\/me\/forge-links$/, () => {
				listed++;
				return reply(listed === 1
					? { forges: [GITLAB], links: [{ id: "fl_1", provider: "gitlab",
						api_url: GITLAB.api_url, forge_user_id: 77, created_at: "2026-10-05T12:00:00Z" }] }
					: { forges: [GITLAB], links: [] });
			}],
		],
	});
	await page.app.loadForgeLinks();
	await page.clock.flush();
	fire(page.document.querySelector("#links button[data-action]"), "click");
	await page.clock.flush();
	assert.equal(page.net.calls.filter((c) => c.method === "DELETE").length, 1, "nothing was deleted");
	assert.equal(page.document.getElementById("links-notice").textContent,
		"Unlinked your GitLab account.");
	assert.equal(page.document.querySelector("#links button[data-action]").textContent,
		"Link GitLab account");
});

test("a link on a forge no longer set up is still listed so it can be removed", async () => {
	const page = await openLinks({
		forges: [],
		links: [{ id: "fl_9", provider: "github", api_url: "https://old.example.com/api/v3",
			forge_user_id: 5, created_at: "2026-10-05T12:00:00Z" }],
	});
	const rows = page.document.querySelectorAll("#links tr");
	assert.equal(rows.length, 1);
	assert.ok(rows[0].textContent.includes("old.example.com"), rows[0].textContent);
	assert.equal(rows[0].querySelector("button[data-action]").textContent, "Unlink");
});

test("the page is in the navigation for every role, named in link tips, with a guide chip", () => {
	const nav = loadParts(ALL_PARTS);
	const item = nav.NAV_GROUPS.flatMap((g) => g.items).find((it) => it.key === "links");
	assert.ok(item, "the navigation has no linked accounts entry");
	assert.equal(item.href, "/ui/links");
	assert.equal(Boolean(item.admin) || Boolean(item.operator), false,
		"a viewer links their own account too");
	assert.equal(nav.PAGE_NAV.links, "links");
	assert.ok(nav.NAV_ICONS.links, "the entry has no icon");
	assert.equal(nav.describeRoute("/ui/links"), "Click to open your linked accounts");
	assert.equal(nav.PAGE_DOCS.links.slug, "pull-request-review");
});

test("a decision made from a pull request comment names the comment the chain committed to", () => {
	const page = loadPage("detail", { vars: { RunID: "run_1" } });
	const line = page.app.decisionLine({
		id: "dec_1", kind: "decision", verdict: "approved", actor: "ops-admin",
		at: "2026-10-05T12:00:00Z",
		comment: { forge: "github", api_url: "https://ghe.example.com/api/v3", repository: "acme/infra",
			pull_request: 12, comment_id: 555, author_id: 4242, body_sha256: "ab12cd" },
	});
	const text = line.querySelector(".decision-comment").textContent;
	assert.equal(text, "Approved from a pull request comment on GitHub (ghe.example.com): " +
		"acme/infra pull request #12, comment 555 by account id 4242. Comment body SHA-256 ab12cd.");
	const gitlab = page.app.decisionLine({
		id: "dec_2", kind: "decision", verdict: "approved", actor: "ops-admin",
		at: "2026-10-05T12:00:00Z",
		comment: { forge: "gitlab", api_url: "https://gitlab.com/api/v4", repository: "platform/net",
			pull_request: 3, comment_id: 9, author_id: 77, body_sha256: "ff00" },
	});
	assert.match(gitlab.querySelector(".decision-comment").textContent,
		/on GitLab \(gitlab\.com\): platform\/net merge request !3, comment 9 by account id 77/);
	const plain = page.app.decisionLine({ id: "dec_3", kind: "decision", verdict: "approved",
		actor: "ops-admin", at: "2026-10-05T12:00:00Z" });
	assert.equal(plain.querySelector(".decision-comment"), null,
		"a decision made in the queue claims a comment");
});
