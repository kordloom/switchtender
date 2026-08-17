// SSO_PENDING_KEY holds the marker proving this tab is the one that started a sign-in.
const SSO_PENDING_KEY = "st_sso_pending";

// beginSSO marks this tab as having started a sign-in, so the token that comes back is answered to
// a request this browser actually made.
function beginSSO() {
	try {
		sessionStorage.setItem(SSO_PENDING_KEY, "1");
	} catch (e) {
		// A browser refusing session storage still signs in; it just cannot prove the round trip.
	}
}

// consumeSSOFragment stores the session token handed back in the URL fragment after single
// sign-on, then strips it from the address bar so it is not left in history or copied by accident.
//
// It is accepted only in the tab that started the sign-in. A fragment is something any link can
// carry, and this used to take one from any page at any time: a link like
// /ui/credentials#access_token=... silently replaced the reader's session with the sender's, scrubbed
// the address bar, and kept the admin navigation rendering because the role came from the fragment
// too. Anything typed afterward, a production secret being the obvious one, was written into the
// sender's account. The marker is per-tab and same-origin, so a link opened from outside carries
// nothing that can satisfy it.
function consumeSSOFragment() {
	if (!location.hash || location.hash.indexOf("access_token=") === -1) return;
	// Read the fragment before stripping it, since replaceState clears it.
	const raw = location.hash.slice(1);
	let started = false;
	try {
		started = sessionStorage.getItem(SSO_PENDING_KEY) === "1";
		sessionStorage.removeItem(SSO_PENDING_KEY);
	} catch (e) {
		started = false;
	}
	// Strip the fragment either way, so a rejected one is not left in the address bar to be
	// copied, shared, or retried.
	history.replaceState(null, "", location.pathname + location.search);
	if (!started) {
		setStatus("Ignored a sign-in link this browser did not ask for. Sign in from this page.");
		return;
	}
	const params = new URLSearchParams(raw);
	const token = params.get("access_token");
	if (!token) return;
	localStorage.setItem("st_token", token);
	if (params.get("role")) localStorage.setItem("st_role", params.get("role"));
	if (params.get("user")) localStorage.setItem("st_user", params.get("user"));
}

// ssoError returns a single sign-on error passed back in the URL fragment, or empty when none.
function ssoError() {
	if (!location.hash || location.hash.indexOf("error=") === -1) return "";
	return new URLSearchParams(location.hash.slice(1)).get("error") || "";
}

// authHeaders builds the Authorization header when a token is stored.
function authHeaders() {
	const token = apiToken();
	return token ? { "Authorization": "Bearer " + token } : {};
}

// requireLogin sends the browser to the sign in page, remembering where it was. The redirect flag
// stops timers such as the welcome tour from firing into the navigation.
function requireLogin() {
	if (document.body.dataset.page === "login") return;
	window.ymRedirecting = true;
	sessionStorage.setItem("st_return", location.pathname);
	location.href = "/ui/login";
}

// uiRole returns the signed-in role, empty when unknown. An open install and an unscoped admin
// token both have no role, and both hold full authority, so an empty role gates nothing.
function uiRole() {
	return localStorage.getItem("st_role") || "";
}

// roleAtLeast reports whether this session may act at the given level. The server enforces the
// real policy; this only decides whether a control is worth drawing, so a button's only future is
// not a 403.
function roleAtLeast(need) {
	const role = uiRole();
	if (!role) return true;
	const rank = { viewer: 1, operator: 2, admin: 3 };
	return (rank[role] || 3) >= (rank[need] || 3);
}

// signedInAs reports whether the named actor is the person holding this session. It compares against
// the identity the server resolved at sign-in, so it is the same name the chain records on the run and
// the same one the approval check compares. An unknown session matches nobody, which errs toward
// drawing a control the server may refuse rather than hiding one the reader is entitled to.
function signedInAs(actor) {
	const me = localStorage.getItem("st_user") || "";
	return !!actor && !!me && actor === me;
}

// getJSON fetches and decodes a JSON endpoint, redirecting to sign in on a 401. A 403 explains
// itself in role terms instead of surfacing the request path and a bare status code, which read
// as breakage rather than as policy.
async function getJSON(url) {
	const res = await fetch(API + url, { headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (res.status === 403) {
		throw new Error("this view needs a higher role than this session holds");
	}
	if (!res.ok) {
		throw new Error(url + " returned " + res.status);
	}
	return res.json();
}

// authedDelete deletes a resource with the session's credentials, translating the failures a
// person can act on: a 401 walks to sign-in, a 403 explains itself in role terms, and anything
// else carries the server's own error text instead of a bare HTTP status.
async function authedDelete(path) {
	const res = await fetch(API + path, { method: "DELETE", headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (res.status === 403) {
		throw new Error("deleting this needs a higher role than this session holds");
	}
	if (!res.ok) {
		const data = await res.json().catch(() => ({}));
		throw new Error(data.error || ("the server refused with HTTP " + res.status));
	}
}

// mountLiveRegions marks every status line as a polite live region so assistive tech announces the
// async success and failure text written into it, including sign-in errors and empty states.
function mountLiveRegions() {
	const regions = document.querySelectorAll('[id="status"], [id$="-status"]');
	for (const el of regions) {
		el.setAttribute("role", "status");
		el.setAttribute("aria-live", "polite");
	}
}

// setStatus shows or clears the status line.
function setStatus(msg) {
	const el = document.getElementById("status");
	if (!el) return;
	el.className = "muted";
	if (msg) { el.textContent = msg; el.hidden = false; } else { el.hidden = true; }
}

// showEmpty renders a centered empty-state card in place of the status line. keepControls leaves
// the filter toolbar visible, for an emptiness that is about the query rather than the instance:
// hiding the controls there took away the search box the person was typing in, which turned a
// no-match search into a dead end only a reload could leave.
function showEmpty(msg, keepControls) {
	const el = document.getElementById("status");
	if (!el) return;
	el.hidden = false;
	el.className = "empty-state";
	el.innerHTML = '<svg viewBox="0 0 24 24" width="36" height="36" fill="none" stroke="currentColor" ' +
		'stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">' +
		'<path d="M3 14h4l2 3h6l2-3h4"/>' +
		'<path d="M5.5 5.5h13l2.5 8.5v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4z"/></svg><p></p>';
	el.querySelector("p").textContent = msg;
	if (keepControls) return;
	// Controls that filter, page, or export an empty list are noise, so they hide with it.
	for (const sel of [".list-filter", ".runs-toolbar", ".table-foot"]) {
		for (const node of document.querySelectorAll(sel)) node.hidden = true;
	}
}

// showListControls reveals the filter, toolbar, and footer that showEmpty hid, for a list that
// turned out to have rows after all.
function showListControls() {
	for (const sel of [".list-filter", ".runs-toolbar"]) {
		for (const node of document.querySelectorAll(sel)) node.hidden = false;
	}
}

// removeRow deletes a table row and restores the empty-state when the last row is gone, so a list
// cleared down to nothing shows its empty message instead of a bare header.
function removeRow(tr, emptyMsg) {
	const body = tr.parentNode;
	tr.remove();
	if (body && body.rows && body.rows.length === 0) {
		const table = body.closest("table");
		if (table) table.hidden = true;
		showEmpty(emptyMsg || "Nothing here yet.");
	}
}

// fmtDuration renders the span between two ISO times.
function fmtDuration(startISO, endISO) {
	if (!startISO || !endISO) return "";
	const ms = new Date(endISO) - new Date(startISO);
	if (isNaN(ms) || ms < 0) return "";
	return fmtMs(ms);
}

// fmtMs renders a millisecond duration.
function fmtMs(ms) {
	if (ms < 1000) return Math.round(ms) + "ms";
	return (ms / 1000).toFixed(1) + "s";
}

// fmtTime renders an ISO time in the local locale.
function fmtTime(iso) {
	if (!iso) return "";
	const d = new Date(iso);
	return isNaN(d) ? iso : d.toLocaleString();
}

// relTime renders an ISO time as a short relative age, such as "2m ago", falling back to the date
// for anything older than a month.
function relTime(iso) {
	if (!iso) return "";
	const d = new Date(iso);
	if (isNaN(d)) return iso;
	const s = Math.round((Date.now() - d.getTime()) / 1000);
	if (s < 5) return "just now";
	if (s < 60) return s + "s ago";
	const m = Math.round(s / 60);
	if (m < 60) return m + "m ago";
	const h = Math.round(m / 60);
	if (h < 24) return h + "h ago";
	const days = Math.round(h / 24);
	if (days < 30) return days + "d ago";
	return d.toLocaleDateString();
}

// baseName returns the last path segment, so a run shows its playbook file rather than a long path.
function baseName(p) {
	if (!p) return "";
	const i = p.lastIndexOf("/");
	return i >= 0 ? p.slice(i + 1) : p;
}

// shortId truncates a long identifier for display, keeping the full value for a tooltip.
function shortId(id) {
	return id && id.length > 15 ? id.slice(0, 13) + "…" : (id || "");
}

// isReadOnly reports whether the server serves a read-only demo, which hides mutating controls.
function isReadOnly() {
	return document.body.dataset.readonly === "true";
}

// aiOff reports whether the server said advisory AI is off for this page. A page without the
// marker counts as on, so nothing changes where the server said nothing.
function aiOff() {
	return document.body.dataset.aiOff === "true";
}

// aiOffNoticeEl builds the standard advisory-AI-off notice: the lead sentence, then the link
// explaining how to turn it on. Every AI surface shows the same notice, so the off state reads
// the same wherever it is met.
function aiOffNoticeEl(lead) {
	const off = document.createElement("div");
	off.className = "ask-off";
	const text = document.createElement("span");
	text.textContent = lead + " ";
	const link = document.createElement("a");
	link.href = "/ui/docs/ai";
	link.className = "link-arrow";
	link.textContent = "How to enable Advisory AI";
	off.appendChild(text);
	off.appendChild(link);
	return off;
}

// auditObjectPages maps an id prefix to the page that shows that object, so an entry naming one
// links to the thing it changed.
const auditObjectPages = {
	run_: "/ui/runs/", tpl_: "/ui/templates", proj_: "/ui/projects", inv_: "/ui/inventories",
	cred_: "/ui/credentials", sch_: "/ui/schedules", usr_: "/ui/users", pol_: "/ui/policies",
	src_: "/ui/sources", trg_: "/ui/schedules",
};

