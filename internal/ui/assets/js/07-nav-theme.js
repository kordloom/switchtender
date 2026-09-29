// buildNav injects the menu toggle and the slide-in drawer on every page but sign in, highlighting
// the current page and hiding admin links from non-admins. The toggle opens and closes the drawer.
function buildNav() {
	if (document.body.dataset.page === "login") return;
	const topbar = document.querySelector(".topbar");
	if (!topbar) return;

	const activeKey = PAGE_NAV[document.body.dataset.page] || "";

	const burger = '<line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/>' +
		'<line x1="3" y1="18" x2="21" y2="18"/>';
	const cross = '<line x1="6" y1="6" x2="18" y2="18"/><line x1="6" y1="18" x2="18" y2="6"/>';

	const toggle = document.createElement("button");
	toggle.className = "nav-toggle";
	toggle.setAttribute("aria-label", "Open menu");
	toggle.setAttribute("aria-expanded", "false");
	toggle.innerHTML = svgIcon(burger);
	topbar.insertBefore(toggle, topbar.firstChild);

	const backdrop = document.createElement("div");
	backdrop.className = "nav-backdrop";
	backdrop.hidden = true;

	const drawer = document.createElement("nav");
	drawer.className = "nav-drawer";
	drawer.hidden = true;
	drawer.setAttribute("aria-label", "Main navigation");

	// fillGroups renders the grouped nav links into a container, shared by the drawer and the
	// docked sidebar so the two never drift apart.
	const fillGroups = (root) => {
		for (const group of NAV_GROUPS) {
			const items = group.items.filter((it) =>
			(!it.admin || roleAtLeast("admin")) && (!it.operator || roleAtLeast("operator")));
			if (!items.length) continue;
			const g = document.createElement("div");
			g.className = "nav-group";
			const gl = document.createElement("div");
			gl.className = "nav-group-label";
			gl.textContent = group.label;
			g.appendChild(gl);
			for (const it of items) {
				const a = document.createElement("a");
				a.className = "nav-item" + (it.key === activeKey ? " active" : "");
				a.href = it.href;
				a.innerHTML = svgIcon(NAV_ICONS[it.key] || "");
				const lbl = document.createElement("span");
				lbl.className = "nav-label";
				lbl.textContent = it.label;
				a.appendChild(lbl);
				if (it.key === activeKey) a.setAttribute("aria-current", "page");
				g.appendChild(a);
			}
			root.appendChild(g);
		}
	};
	fillGroups(drawer);
	drawer.appendChild(accountGroup());
	drawer.appendChild(themeGroup());

	const side = document.createElement("aside");
	side.className = "side";
	const sideBrand = document.createElement("a");
	sideBrand.className = "side-brand";
	sideBrand.href = "/ui/";
	sideBrand.innerHTML = '<picture><source media="(prefers-color-scheme: dark)" srcset="/ui/assets/logo-train-tracks-dark.png"><img src="/ui/assets/logo-train-tracks.png" alt=""></picture><span class="side-brand-word">SwitchTender</span>';
	side.appendChild(sideBrand);
	const sideNav = document.createElement("nav");
	sideNav.setAttribute("aria-label", "Primary navigation");
	fillGroups(sideNav);
	side.appendChild(sideNav);
	side.appendChild(accountGroup());
	side.appendChild(themeGroup());

	// Collapse control: shrink the docked sidebar to an icon rail and back, remembered per browser.
	// Collapsed, each item's label moves to a hover tip, so the icons stay reachable without the text.
	const collapseBtn = document.createElement("button");
	collapseBtn.type = "button";
	collapseBtn.className = "side-collapse";
	const chevron = (collapsed) => svgIcon(collapsed
		? '<polyline points="9 18 15 12 9 6"/>'
		: '<polyline points="15 18 9 12 15 6"/>') + '<span class="nav-label">Collapse</span>';
	const applyCollapsed = (v) => {
		document.body.dataset.navCollapsed = v ? "true" : "false";
		collapseBtn.setAttribute("aria-label", v ? "Expand navigation" : "Collapse navigation");
		collapseBtn.setAttribute("aria-pressed", v ? "true" : "false");
		collapseBtn.innerHTML = chevron(v);
		side.querySelectorAll(".nav-item").forEach((a) => {
			const l = a.querySelector(".nav-label");
			if (v && l) a.dataset.tip = l.textContent;
			else delete a.dataset.tip;
		});
	};
	collapseBtn.addEventListener("click", () => {
		const next = document.body.dataset.navCollapsed !== "true";
		localStorage.setItem("st_nav_collapsed", next ? "1" : "0");
		applyCollapsed(next);
	});
	side.appendChild(collapseBtn);
	applyCollapsed(localStorage.getItem("st_nav_collapsed") === "1");

	document.body.appendChild(backdrop);
	document.body.appendChild(drawer);
	document.body.appendChild(side);
	syncThemeButtons();
	syncBrandLogos();

	// Pin the drawer directly under the top bar, tracking its height across zoom and resize.
	const syncHeight = () => document.documentElement.style
		.setProperty("--topbar-h", topbar.offsetHeight + "px");
	syncHeight();
	window.addEventListener("resize", syncHeight);

	let isOpen = false;
	const setOpen = (v) => {
		isOpen = v;
		backdrop.hidden = !v;
		drawer.hidden = !v;
		document.body.classList.toggle("nav-open", v);
		toggle.setAttribute("aria-expanded", v ? "true" : "false");
		toggle.setAttribute("aria-label", v ? "Close menu" : "Open menu");
		toggle.innerHTML = svgIcon(v ? cross : burger);
		if (v) { const first = drawer.querySelector(".nav-item"); if (first) first.focus(); }
	};
	toggle.addEventListener("click", () => setOpen(!isOpen));
	backdrop.addEventListener("click", () => setOpen(false));
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && isOpen) { setOpen(false); toggle.focus(); }
	});
}

// mountFooter closes every page with a slim bar, so scrolling ends on a deliberate edge rather
// than on the last row of content. It is a sibling of the main column rather than its last child,
// so a short page such as Migrate rests it on the bottom of the viewport instead of floating it up
// under the content with dead space beneath.
function mountFooter() {
	if (document.body.dataset.page === "login" || document.querySelector(".app-foot")) return;
	const main = document.querySelector("main.content");
	if (!main) return;
	const foot = document.createElement("footer");
	foot.className = "app-foot";
	const inner = document.createElement("div");
	inner.className = "app-foot-inner";
	const left = document.createElement("span");
	left.className = "app-foot-brand";
	left.textContent = "SwitchTender";
	const links = document.createElement("nav");
	links.className = "app-foot-links";
	const add = (label, href, tip, external) => {
		const a = document.createElement("a");
		a.href = href;
		a.textContent = label;
		a.dataset.tip = tip;
		if (external) { a.target = "_blank"; a.rel = "noopener"; }
		links.appendChild(a);
	};
	add("Docs", "/ui/docs", "Click to open the documentation");
	// The doctor reads management data, so the link only shows where the page would answer.
	if (roleAtLeast("admin")) add("Doctor", "/ui/doctor", "Click to run the reference health checks");
	add("Source", "https://github.com/kordloom/switchtender", "Click to open the repository", true);
	add("License", "https://github.com/kordloom/switchtender/blob/main/LICENSE", "Click to read the license", true);
	const top = document.createElement("button");
	top.type = "button";
	top.className = "app-foot-top";
	top.textContent = "Back to top";
	top.dataset.tip = "Click to return to the top of this page";
	top.addEventListener("click", () => window.scrollTo({ top: 0, behavior: "smooth" }));
	links.appendChild(top);
	inner.appendChild(left);
	inner.appendChild(links);
	foot.appendChild(inner);
	main.insertAdjacentElement("afterend", foot);
}

// mountDocsChrome adds the reading aids a documentation page needs: a filter over the guide list,
// an on-this-page rail built from the headings, and previous and next links at the end.
function mountDocsChrome() {
	const list = document.getElementById("docs-list");
	const filter = document.getElementById("docs-filter");
	const empty = document.getElementById("docs-empty");
	if (filter && list) {
		filter.addEventListener("input", () => {
			const q = filter.value.trim().toLowerCase();
			let shown = 0;
			for (const li of list.querySelectorAll("li")) {
				const match = q === "" || li.textContent.toLowerCase().includes(q);
				li.hidden = !match;
				if (match) shown++;
			}
			if (empty) empty.hidden = shown > 0;
		});
		filter.addEventListener("keydown", (e) => {
			if (e.key !== "Enter") return;
			const first = list.querySelector("li:not([hidden]) a");
			if (first) location.href = first.getAttribute("href");
		});
	}

	// The on-this-page rail. Headings get ids so each entry can link to its section.
	const toc = document.getElementById("docs-toc");
	const article = document.querySelector(".docs-content");
	if (toc && article) {
		const heads = Array.from(article.querySelectorAll("h2"));
		if (heads.length >= 2) {
			const label = document.createElement("div");
			label.className = "nav-group-label";
			label.textContent = "On this page";
			toc.appendChild(label);
			const ul = document.createElement("ul");
			heads.forEach((h, i) => {
				if (!h.id) {
					h.id = "section-" + (h.textContent.toLowerCase().replace(/[^a-z0-9]+/g, "-")
						.replace(/^-|-$/g, "") || i);
				}
				const li = document.createElement("li");
				const a = document.createElement("a");
				a.href = "#" + h.id;
				a.textContent = h.textContent;
				a.dataset.tip = "Click to jump to this section";
				li.appendChild(a);
				ul.appendChild(li);
			});
			toc.appendChild(ul);
			// The rail follows the reader: the heading nearest the top of the viewport is current.
			const marks = Array.from(toc.querySelectorAll("a"));
			const sync = () => {
				let active = 0;
				heads.forEach((h, i) => {
					if (h.getBoundingClientRect().top <= 120) active = i;
				});
				marks.forEach((m, i) => m.classList.toggle("active", i === active));
			};
			document.addEventListener("scroll", sync, { passive: true });
			sync();
		} else {
			toc.hidden = true;
		}
	}

	// Previous and next, read from the guide list so the order always matches the sidebar.
	const pager = document.getElementById("docs-pager");
	if (pager && list) {
		const links = Array.from(list.querySelectorAll("a"));
		const at = links.findIndex((a) => a.classList.contains("active"));
		if (at !== -1) {
			const add = (link, rel) => {
				if (!link) return;
				const a = document.createElement("a");
				a.className = "docs-pager-link " + rel;
				a.href = link.getAttribute("href");
				const kicker = document.createElement("span");
				kicker.className = "docs-pager-kicker";
				kicker.textContent = rel === "prev" ? "Previous" : "Next";
				const title = document.createElement("span");
				title.className = "docs-pager-title";
				title.textContent = link.textContent;
				a.appendChild(kicker);
				a.appendChild(title);
				a.dataset.tip = "Click to open " + link.textContent;
				pager.appendChild(a);
			};
			add(links[at - 1], "prev");
			add(links[at + 1], "next");
		}
	}
}

// svgIcon wraps inner SVG markup in a stroked 24 by 24 icon that inherits the current color.
function svgIcon(inner) {
	return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" ' +
		'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + inner + '</svg>';
}

// THEMES lists the selectable appearances: the signature look, then the flat family themes.
const THEMES = [
	{ key: "signature", label: "Loom", desc: "The signature look, depth and glow", tip: "Loom, the signature theme" },
	{ key: "light", label: "Linen", desc: "Clean white, the kordloom.com style", tip: "Linen, the white theme" },
	{ key: "dark", label: "Ink", desc: "Warm black, the loomseal.com style", tip: "Ink, the dark theme" },
];

// currentTheme returns the active theme key, defaulting to the signature look.
function currentTheme() {
	const t = document.body.dataset.theme;
	return t === "light" || t === "dark" ? t : "signature";
}

// setTheme applies and persists a theme choice, then refreshes every switcher control.
function setTheme(key) {
	if (key === "light" || key === "dark") document.body.dataset.theme = key;
	else delete document.body.dataset.theme;
	try {
		if (key === "light" || key === "dark") localStorage.setItem("st_theme", key);
		else localStorage.removeItem("st_theme");
	} catch { /* storage may be unavailable */ }
	syncThemeButtons();
	syncBrandLogos();
}

// syncThemeButtons marks the active theme on every switcher segment.
function syncThemeButtons() {
	const active = currentTheme();
	for (const btn of document.querySelectorAll(".theme-btn")) {
		const on = btn.dataset.themeKey === active;
		btn.classList.toggle("active", on);
		btn.setAttribute("aria-pressed", on ? "true" : "false");
	}
}

// themeGroup builds the compact appearance row used at the bottom of the drawer and the
// sidebar: a muted gear hint and one icon button per theme, named by tooltip.
function themeGroup() {
	const g = document.createElement("div");
	g.className = "theme-group";
	const hint = document.createElement("span");
	hint.className = "theme-hint";
	hint.textContent = "Theme";
	g.appendChild(hint);
	const row = document.createElement("div");
	row.className = "theme-row";
	for (const t of THEMES) {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "theme-btn";
		btn.dataset.themeKey = t.key;
		btn.dataset.tip = t.tip;
		btn.setAttribute("aria-label", t.tip);
		btn.setAttribute("aria-pressed", "false");
		btn.textContent = t.label;
		btn.addEventListener("click", () => setTheme(t.key));
		row.appendChild(btn);
	}
	g.appendChild(row);
	return g;
}

// accountGroup builds the who-am-I block that sits above the theme switcher: the signed-in name and
// role, and the control that ends the session. A signed-out browser is offered the way in instead.
//
// Nothing in the interface used to say who you were or let you stop being them. Sign-in was reachable
// only by being thrown at it by a 401, so an install shared by an operator and an auditor had no way
// to hand the browser over, and a session left open on a shared machine could not be ended from the
// interface it was open in.
function accountGroup() {
	const g = document.createElement("div");
	g.className = "account-group";
	const who = document.createElement("div");
	who.className = "account-who";
	g.appendChild(who);

	const name = localStorage.getItem("st_user") || "";
	if (!apiToken()) {
		who.textContent = "Not signed in";
		const link = document.createElement("a");
		link.className = "account-link";
		link.href = "/ui/login";
		link.textContent = "Sign in";
		g.appendChild(link);
		return g;
	}

	const label = document.createElement("span");
	label.className = "account-name";
	// A token session has no username to show, so it is named for what it is rather than left blank.
	label.textContent = name || "API token session";
	who.appendChild(label);
	const role = uiRole();
	if (role) {
		const badge = document.createElement("span");
		badge.className = "account-role";
		badge.textContent = role;
		who.appendChild(badge);
	}

	const out = document.createElement("button");
	out.type = "button";
	out.className = "account-signout";
	out.textContent = "Sign out";
	out.dataset.tip = "Click to end this session on the server and return to sign in";
	out.addEventListener("click", () => signOut());
	g.appendChild(out);
	return g;
}

// signOut ends the session on the server, then clears what this browser remembers and returns to
// sign in. The server call is what actually revokes the credential; clearing storage alone left a
// token that stayed valid for the rest of its thirty days in anyone's hands. The local clear happens
// even when the call fails, because a browser that cannot reach the server must still be able to
// stop presenting the session.
async function signOut() {
	window.ymRedirecting = true;
	try {
		await fetch(API + "/auth/logout", { method: "POST", headers: authHeaders() });
	} catch (e) {
		// An unreachable server does not keep a person signed in on this machine.
	}
	localStorage.removeItem("st_token");
	localStorage.removeItem("st_role");
	localStorage.removeItem("st_user");
	try {
		sessionStorage.removeItem("st_return");
	} catch (e) {
		// A browser refusing session storage has nothing to clear.
	}
	location.href = "/ui/login";
}

// paletteState holds the command palette's elements once built, plus the filtered entries and the
// highlighted index.
let paletteState = null;

// paletteEntries returns everything the palette can jump to: each nav destination the current role
// can see, then a few direct actions.
function paletteEntries() {
	const out = [];
	for (const group of NAV_GROUPS) {
		for (const it of group.items) {
			if (it.admin && !roleAtLeast("admin")) continue;
			if (it.operator && !roleAtLeast("operator")) continue;
			out.push({ label: it.label, desc: it.desc || "", group: group.label, icon: NAV_ICONS[it.key] || "", href: it.href });
		}
	}
	out.push({ label: "Launch a run", desc: "Open the launch panel on the runs page", group: "Action", icon: NAV_ICONS.runs, href: "/ui/runs" });
	out.push({
		label: "View the source", desc: "github.com/kordloom/switchtender", group: "Action",
		icon: '<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/>',
		href: "https://github.com/kordloom/switchtender", external: true,
	});
	for (const t of THEMES) {
		out.push({
			label: "Theme: " + t.label, desc: t.desc, group: "Theme",
			icon: '<circle cx="12" cy="12" r="9"/><path d="M12 3a9 9 0 0 1 0 18z" fill="currentColor" stroke="none"/>',
			action: () => setTheme(t.key),
		});
	}
	return out;
}

// paletteScore ranks an entry against a query: label prefix beats label substring beats
// description, and zero filters the entry out.
function paletteScore(entry, q) {
	if (!q) return 1;
	const label = entry.label.toLowerCase();
	if (label.startsWith(q)) return 4;
	if (label.includes(q)) return 3;
	if (entry.desc.toLowerCase().includes(q)) return 2;
	if (entry.group.toLowerCase().includes(q)) return 1;
	return 0;
}

// buildPalette constructs the palette dialog once and wires its input and keyboard handling.
function buildPalette() {
	if (paletteState) return paletteState;
	const overlay = document.createElement("div");
	overlay.className = "cmdk";
	overlay.hidden = true;
	overlay.setAttribute("role", "dialog");
	overlay.setAttribute("aria-modal", "true");
	overlay.setAttribute("aria-label", "Command palette");
	const card = document.createElement("div");
	card.className = "cmdk-card";
	const head = document.createElement("div");
	head.className = "cmdk-head";
	head.innerHTML = svgIcon('<circle cx="11" cy="11" r="7"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>');
	const input = document.createElement("input");
	input.className = "cmdk-input";
	input.placeholder = "Jump to a page or action…";
	input.setAttribute("aria-label", "Search pages and actions");
	head.appendChild(input);
	const list = document.createElement("div");
	list.className = "cmdk-list";
	list.setAttribute("role", "listbox");
	const foot = document.createElement("div");
	foot.className = "cmdk-foot";
	foot.innerHTML = '<span><span class="kbd">↑↓</span> navigate</span>' +
		'<span><span class="kbd">↵</span> open</span><span><span class="kbd">esc</span> close</span>';
	card.appendChild(head);
	card.appendChild(list);
	card.appendChild(foot);
	overlay.appendChild(card);
	document.body.appendChild(overlay);
	overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) closePalette(); });
	input.addEventListener("input", () => renderPalette(input.value));
	input.addEventListener("keydown", (e) => {
		if (e.key === "ArrowDown") { e.preventDefault(); movePaletteActive(1); }
		else if (e.key === "ArrowUp") { e.preventDefault(); movePaletteActive(-1); }
		else if (e.key === "Enter") { e.preventDefault(); openPaletteActive(); }
		else if (e.key === "Escape") { e.preventDefault(); closePalette(); }
	});
	paletteState = { overlay, input, list, active: 0, shown: [] };
	return paletteState;
}

// renderPalette filters the entries for a query and redraws the result list.
function renderPalette(query) {
	const st = paletteState;
	const q = (query || "").trim().toLowerCase();
	st.shown = paletteEntries()
		.map((entry) => ({ entry, score: paletteScore(entry, q) }))
		.filter((r) => r.score > 0)
		.sort((a, b) => b.score - a.score)
		.map((r) => r.entry);
	st.active = 0;
	st.list.innerHTML = "";
	if (!st.shown.length) {
		const none = document.createElement("div");
		none.className = "cmdk-empty";
		none.textContent = "No matches.";
		st.list.appendChild(none);
		return;
	}
	st.shown.forEach((entry, i) => {
		const item = document.createElement("div");
		item.className = "cmdk-item" + (i === st.active ? " active" : "");
		item.setAttribute("role", "option");
		item.setAttribute("aria-selected", i === st.active ? "true" : "false");
		item.innerHTML = svgIcon(entry.icon) +
			'<span class="cmdk-item-label"></span><span class="cmdk-item-desc"></span><span class="cmdk-item-group"></span>';
		item.querySelector(".cmdk-item-label").textContent = entry.label;
		item.querySelector(".cmdk-item-desc").textContent = entry.desc;
		item.querySelector(".cmdk-item-group").textContent = entry.group;
		item.addEventListener("mouseenter", () => setPaletteActive(i));
		item.addEventListener("click", () => { st.active = i; openPaletteActive(); });
		st.list.appendChild(item);
	});
}

// setPaletteActive moves the highlight to the given index.
function setPaletteActive(i) {
	const st = paletteState;
	st.active = i;
	st.list.querySelectorAll(".cmdk-item").forEach((el, j) => {
		el.classList.toggle("active", j === i);
		el.setAttribute("aria-selected", j === i ? "true" : "false");
	});
}

// movePaletteActive steps the highlight up or down, wrapping, and keeps it scrolled into view.
function movePaletteActive(delta) {
	const st = paletteState;
	if (!st.shown.length) return;
	const next = (st.active + delta + st.shown.length) % st.shown.length;
	setPaletteActive(next);
	const el = st.list.querySelectorAll(".cmdk-item")[next];
	if (el) el.scrollIntoView({ block: "nearest" });
}

// openPaletteActive navigates to the highlighted entry.
function openPaletteActive() {
	const st = paletteState;
	const entry = st.shown[st.active];
	if (!entry) return;
	closePalette();
	if (entry.action) entry.action();
	else if (entry.external) window.open(entry.href, "_blank", "noopener");
	else location.href = entry.href;
}

// openPalette shows the palette with a fresh query and focuses its input.
function openPalette() {
	const st = buildPalette();
	st.overlay.hidden = false;
	st.input.value = "";
	renderPalette("");
	st.input.focus();
}

// closePalette hides the palette.
function closePalette() {
	if (paletteState) paletteState.overlay.hidden = true;
}

// ROUTE_WORDS names each interface route for the hover tip, so a link says what it opens rather
// than printing its path. A trailing id is matched by the pattern, not spelled out.
const ROUTE_WORDS = [
	[/^\/ui\/runs\/[^/]+$/, "Click to open this run"],
	[/^\/ui\/runs$/, "Click to open the runs list"],
	[/^\/ui\/hosts\/[^/]+$/, "Click to open this host's history"],
	[/^\/ui\/fleet$/, "Click to open fleet health"],
	[/^\/ui\/drift$/, "Click to open drift"],
	[/^\/ui\/tasks$/, "Click to open task trends"],
	[/^\/ui\/workers$/, "Click to open workers"],
	[/^\/ui\/projects$/, "Click to open projects"],
	[/^\/ui\/inventories$/, "Click to open inventories"],
	[/^\/ui\/sources$/, "Click to open inventory sources"],
	[/^\/ui\/templates$/, "Click to open templates"],
	[/^\/ui\/workflows$/, "Click to open the workflow editor"],
	[/^\/ui\/schedules$/, "Click to open schedules"],
	[/^\/ui\/migrate$/, "Click to open the migration importer"],
	[/^\/ui\/credentials$/, "Click to open credentials"],
	[/^\/ui\/users$/, "Click to open users"],
	[/^\/ui\/audit$/, "Click to open the audit trail"],
	[/^\/ui\/policies$/, "Click to open approval policies"],
	[/^\/ui\/doctor$/, "Click to run the reference health checks"],
	[/^\/ui\/docs\/[^/]+$/, "Click to open this guide"],
	[/^\/ui\/docs$/, "Click to open the documentation"],
	[/^\/ui\/?$/, "Click to open the overview"],
];

// describeRoute returns the sentence for an interface path, empty when nothing matches.
function describeRoute(path) {
	for (const [pattern, words] of ROUTE_WORDS) {
		if (pattern.test(path)) return words;
	}
	return "";
}

// wireHinttips shows a floating tip above any element carrying data-tip, on hover and keyboard
// focus. One shared element rides document.body, so no scroll container can clip it, and it is
// clamped to the viewport.
function wireHinttips() {
	const tip = document.createElement("div");
	tip.className = "hinttip";
	tip.hidden = true;
	document.body.appendChild(tip);
	let linkTimer = 0;
	const place = (target, text) => {
		tip.textContent = text;
		// A tip carrying newlines is a small block of explanation rather than a label, so it wraps on
		// its own lines instead of running off the edge of the viewport.
		tip.classList.toggle("hinttip-block", text.includes("\n"));
		tip.hidden = false;
		tip.style.left = "0px";
		tip.style.top = "0px";
		const r = target.getBoundingClientRect();
		const x = Math.min(Math.max(8, r.left), window.innerWidth - tip.offsetWidth - 8);
		let y = r.top - tip.offsetHeight - 8;
		if (y < 8) y = r.bottom + 8;
		tip.style.left = x + "px";
		tip.style.top = y + "px";
	};
	// linkDest says what a link does in plain words. A raw path tells a reader nothing they
	// cannot already see in the status bar, so internal destinations are named by what they are.
	const linkDest = (a) => {
		const href = a.getAttribute("href");
		if (!href || href === "#" || href.startsWith("javascript")) return "";
		try {
			const u = new URL(href, location.href);
			if (u.origin !== location.origin) return "Click to open " + u.hostname;
			if (a.hasAttribute("download")) return "Click to download this file";
			return describeRoute(u.pathname);
		} catch { return ""; }
	};
	const show = (e) => {
		if (!e.target.closest) return;
		// In the read-only demo a mutating control still teaches what it would do, then says why
		// nothing happens when it is pressed.
		const blocked = isReadOnly() && e.target.closest("[data-mutates]");
		if (blocked) {
			clearTimeout(linkTimer);
			const what = blocked.dataset.tip || "This changes data";
			place(blocked, what + ". Disabled in this read-only demo");
			return;
		}
		const target = e.target.closest("[data-tip]");
		if (target) { clearTimeout(linkTimer); place(target, target.dataset.tip); return; }
		// Plain links reveal their destination after a beat, so casual mouse travel stays quiet.
		const a = e.target.closest("a[href]");
		if (!a || a.closest(".cmdk")) return;
		const dest = linkDest(a);
		if (!dest) return;
		clearTimeout(linkTimer);
		linkTimer = window.setTimeout(() => place(a, dest), 450);
	};
	const hide = (e) => {
		if (!e.target.closest) return;
		if (e.target.closest("[data-tip]") || e.target.closest("a[href]")) {
			clearTimeout(linkTimer);
			tip.hidden = true;
		}
	};
	document.addEventListener("mouseover", show);
	document.addEventListener("mouseout", hide);
	document.addEventListener("focusin", show);
	document.addEventListener("focusout", hide);
	document.addEventListener("scroll", () => { clearTimeout(linkTimer); tip.hidden = true; }, true);
	document.addEventListener("click", () => { clearTimeout(linkTimer); tip.hidden = true; }, true);
	document.documentElement.addEventListener("mouseleave", () => { clearTimeout(linkTimer); tip.hidden = true; });
}

// wirePalette registers the platform search shortcut that toggles the palette, on every page but
// sign in.
function wirePalette() {
	if (document.body.dataset.page === "login") return;
	document.addEventListener("keydown", (e) => {
		if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "k") {
			e.preventDefault();
			if (paletteState && !paletteState.overlay.hidden) closePalette();
			else openPalette();
		}
	});
}

// apiToken returns the stored API token, empty when the server runs open.
function apiToken() {
	return localStorage.getItem("st_token") || "";
}

