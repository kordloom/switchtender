// FORGE_NAMES maps a forge provider to the name a person knows it by.
const FORGE_NAMES = { github: "GitHub", gitlab: "GitLab" };

// forgeName returns the display name of a forge provider.
function forgeName(provider) {
	return FORGE_NAMES[provider] || String(provider || "");
}

// forgeHostOf returns the host of a forge address, or the address itself when it has none.
function forgeHostOf(address) {
	try {
		return new URL(address).host;
	} catch {
		return address || "";
	}
}

// takeLinkNotice reads what the link callback said in the address fragment, a forge linked or why
// nothing was, and strips the fragment so a reload does not say it again. It returns the sentence
// to show, empty when the fragment says nothing about a link.
function takeLinkNotice() {
	const hash = location.hash || "";
	if (hash.indexOf("linked=") === -1 && hash.indexOf("error=") === -1) return "";
	const params = new URLSearchParams(hash.slice(1));
	history.replaceState(null, "", location.pathname + location.search);
	if (params.get("error")) return "Nothing was linked: " + params.get("error");
	const host = params.get("host");
	return "Linked your " + forgeName(params.get("linked")) + " account" +
		(host ? " on " + host : "") + ".";
}

// showLinkNotice writes a sentence above the table, or hides the line when there is nothing to say.
function showLinkNotice(text) {
	const el = document.getElementById("links-notice");
	if (!el) return;
	el.textContent = text;
	el.hidden = !text;
}

// loadForgeLinks draws each forge this server can link an account on, the account linked there, and
// the action that changes it. A link on a forge no longer set up is still listed, so it can be
// removed.
async function loadForgeLinks() {
	const notice = takeLinkNotice();
	if (notice) showLinkNotice(notice);
	try {
		const data = await getJSON("/me/forge-links");
		const forges = Array.isArray(data.forges) ? data.forges : [];
		const links = Array.isArray(data.links) ? data.links : [];
		const tbody = document.getElementById("links");
		tbody.textContent = "";
		if (forges.length === 0 && links.length === 0) {
			showEmpty("No forge is set up for linking on this server. An administrator adds GitHub or " +
				"GitLab with --forge-oauth, as the pull request review guide describes.");
			return;
		}
		const used = new Set();
		for (const f of forges) {
			const link = links.find((l) => l.provider === f.provider && l.api_url === f.api_url);
			if (link) used.add(link.id);
			tbody.appendChild(forgeLinkRow(f.provider, f.host || forgeHostOf(f.web_url), link, f));
		}
		for (const l of links) {
			if (used.has(l.id)) continue;
			tbody.appendChild(forgeLinkRow(l.provider, forgeHostOf(l.api_url), l, null));
		}
		setStatus("");
		document.getElementById("links-table").hidden = false;
	} catch (e) {
		setStatus("Failed to load linked accounts: " + e.message);
	}
}

// forgeLinkRow builds one forge's row: the forge, its host, the linked account by its numeric id,
// when it was linked, and a Link or an Unlink button. forge is null for a link whose forge is no
// longer set up, which can only be removed.
function forgeLinkRow(provider, host, link, forge) {
	const tr = document.createElement("tr");
	tr.appendChild(td(forgeName(provider)));
	tr.appendChild(td(host, "mono"));
	tr.appendChild(link ? td("Account id " + link.forge_user_id, "mono") : td("Not linked", "muted"));
	tr.appendChild(link ? tdTime(link.created_at) : td(""));
	const cell = document.createElement("td");
	const button = document.createElement("button");
	button.type = "button";
	button.dataset.action = link ? "unlink" : "link";
	if (link) {
		button.className = "button danger";
		button.textContent = "Unlink";
		button.addEventListener("click", () => unlinkForge(link, button));
	} else if (forge) {
		button.className = "button primary";
		button.textContent = "Link " + forgeName(provider) + " account";
		button.addEventListener("click", () => startForgeLink(forge, button));
	}
	if (link || forge) cell.appendChild(button);
	tr.appendChild(cell);
	return tr;
}

// startForgeLink asks the server for the forge's sign-in address and sends the browser there. The
// forge sends it back to the server, which links the account and returns here.
async function startForgeLink(forge, button) {
	button.disabled = true;
	try {
		const answer = await postForAnswer("/me/forge-links",
			{ provider: forge.provider, api_url: forge.api_url });
		if (answer.status !== 200 || !answer.body.authorize_url) {
			throw new Error(answer.body.error || ("HTTP " + answer.status));
		}
		location.href = answer.body.authorize_url;
	} catch (err) {
		showLinkNotice("Could not start linking: " + err.message);
		button.disabled = false;
	}
}

// unlinkForge removes one link once the person confirms. A comment from that forge account stops
// acting as them at once.
async function unlinkForge(link, button) {
	const ask = "Unlink this " + forgeName(link.provider) + " account? Comments it writes will no " +
		"longer act as you.";
	if (!window.confirm(ask)) return;
	button.disabled = true;
	try {
		await authedDelete("/me/forge-links/" + encodeURIComponent(link.id));
		showLinkNotice("Unlinked your " + forgeName(link.provider) + " account.");
		await loadForgeLinks();
	} catch (err) {
		showLinkNotice("Could not unlink: " + err.message);
		button.disabled = false;
	}
}
