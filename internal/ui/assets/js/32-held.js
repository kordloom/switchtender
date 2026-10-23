// The held page: every change waiting for a person, or with ?who=agent only the runs an AI agent asked
// for, each saying why it is held, who can decide it, and what each answer runs.
//
// The items come from GET /v1/attention, the same answer the overview's Needs attention panel draws,
// and each is drawn by attentionItem, so this page and the overview cannot describe a held change
// differently. The runs list answers a different question, every run with install-wide counts, which
// is why a visitor sent there to see the gate met three numbers for one question.

// heldWho returns which held changes the page shows: "agent" for the runs an AI agent asked for, and
// "" for everything waiting on a person.
function heldWho() {
	return new URLSearchParams(location.search).get("who") === "agent" ? "agent" : "";
}

// heldByAgent reports whether an item is held by the built-in hold on a run an agent asked for.
function heldByAgent(it) {
	const approval = (it.main || {}).approval || {};
	return approval.held_by === AGENT_DEFAULT_HOLD;
}

// loadHeld reads what is waiting for a person and draws the page.
async function loadHeld() {
	const who = heldWho();
	renderHeldHead(who);
	let data;
	try {
		data = await getJSON("/attention?blocker=approval_needed");
	} catch (err) {
		setStatus("Could not read what is waiting for a person: " + err.message);
		return;
	}
	// An answer without its list is a failed read, not an empty one: saying nothing is waiting
	// would tell a visitor the gate is idle when the server never said so.
	if (!data || !Array.isArray(data.items)) {
		setStatus("Could not read what is waiting for a person: the server sent no list.");
		return;
	}
	renderHeld(who, data);
}

// renderHeldHead sets the title and the lede for the set the page shows.
function renderHeldHead(who) {
	const title = document.getElementById("held-title");
	const lede = document.getElementById("held-lede");
	const how = document.getElementById("held-how");
	if (who !== "agent") return;
	document.title = "An AI agent asked - SwitchTender";
	if (title) title.textContent = "An AI agent asked. A person decides.";
	if (lede) {
		lede.textContent = "An AI agent asked for these changes with its own token. SwitchTender held " +
			"each one before anything ran, with no policy written: a run an agent asks for waits for a " +
			"person by default, and an agent can never approve its own request.";
	}
	if (how) how.hidden = false;
}

// renderHeld draws the count line, the items, how a decision is made, and where to go next.
function renderHeld(who, data) {
	const all = (data.items || []).filter((it) => it.blocker === "approval_needed");
	const items = who === "agent" ? all.filter(heldByAgent) : all;
	const total = Number((data.counts || {}).approval_needed) || all.length;
	const list = document.getElementById("held-list");
	if (list) {
		list.textContent = "";
		for (const it of items) list.appendChild(attentionItem(it));
	}
	renderHeldCount(who, items.length, total);
	if (!items.length) {
		showEmpty(who === "agent"
			? "No run an AI agent asked for is waiting right now."
			: "Nothing is waiting for a person right now.", true);
	} else {
		const status = document.getElementById("status");
		if (status) status.hidden = true;
	}
	renderHeldDecide(items.length);
	renderHeldNext(items);
}

// renderHeldCount says how many changes the page shows and, when it shows only the agent's, how many
// are waiting in all, linked to the page that shows them, so the two numbers each say what they count.
function renderHeldCount(who, shown, total) {
	const el = document.getElementById("held-count");
	if (!el) return;
	el.textContent = "";
	if (who !== "agent") {
		el.append(shown + " " + plural(shown, "change is", "changes are") + " waiting for a person.");
		el.hidden = false;
		return;
	}
	el.append(shown + " " + plural(shown, "run an AI agent asked for is", "runs an AI agent asked for are") +
		" waiting for a person.");
	if (total > shown) {
		const link = document.createElement("a");
		link.href = "/ui/held";
		link.textContent = "See all " + total + " waiting for a person on this install";
		el.append(" ", link, ".");
	}
	el.hidden = false;
}

// renderHeldDecide says where a decision is made. The page has no decision control: on a read-only
// server it says why and what a writable install does, and elsewhere it points at the run page,
// where the decision lives with the risk and the spec it releases.
function renderHeldDecide(shown) {
	const el = document.getElementById("held-decide");
	if (!el) return;
	if (!shown) {
		el.hidden = true;
		return;
	}
	const how = "An admin opens the run and presses Approve or Deny there. The decision is written to the " +
		"audit chain with a digest of the exact spec it released.";
	if (document.body.dataset.readonly === "true") {
		el.className = "ro-note held-decide";
		el.textContent = readOnlyReason() + ". On a writable install, " + how.charAt(0).toLowerCase() +
			how.slice(1);
	} else {
		el.className = "muted held-decide";
		el.textContent = how;
	}
	el.hidden = false;
}

// renderHeldNext lists where a visitor goes from here: the first held run, the change register, and on
// the hosted demo the site's section on checking the history yourself.
function renderHeldNext(items) {
	const section = document.getElementById("held-next");
	const ul = document.getElementById("held-next-links");
	if (!section || !ul) return;
	ul.textContent = "";
	const add = (href, text) => {
		const li = document.createElement("li");
		const a = document.createElement("a");
		a.href = href;
		a.textContent = text;
		li.appendChild(a);
		ul.appendChild(li);
	};
	const first = items.find((it) => it.run_id);
	if (first) add("/ui/runs/" + encodeURIComponent(first.run_id), "Open the first held run");
	add("/ui/audit/register", "Read the change register");
	if (isDemo() && location.hostname === PUBLIC_DEMO_HOST) {
		add("https://switchtender.com/#proof", "Check the history yourself");
	}
	section.hidden = false;
}
