// PAGE_NAV maps a page identifier to the nav key it should highlight.
const PAGE_NAV = {
	overview: "overview", runs: "runs", detail: "runs", fleet: "fleet", host: "fleet",
	tasks: "tasks", workers: "workers", drift: "drift", projects: "projects", inventories: "inventories",
	sources: "sources", jobtemplates: "templates", schedules: "schedules", workflows: "workflows",
	migrate: "migrate", credentials: "credentials", users: "users", audit: "audit",
	policies: "policies", doctor: "doctor", docs: "docs",
};

// NAV_ICONS holds the inline SVG body for each nav key, stroked in the current color.
const NAV_ICONS = {
	overview: '<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/>',
	runs: '<circle cx="12" cy="12" r="9"/><polygon points="10 8 16 12 10 16"/>',
	fleet: '<path d="M3 12h4l2 6 4-12 2 6h6"/>',
	drift: '<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>',
	tasks: '<path d="M3 17l6-6 4 4 8-8"/><path d="M17 7h4v4"/>',
	workers: '<rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><line x1="7" y1="7.5" x2="7.01" y2="7.5"/><line x1="7" y1="16.5" x2="7.01" y2="16.5"/>',
	projects: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
	inventories: '<line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/>',
	sources: '<path d="M23 4v6h-6"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/>',
	templates: '<polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>',
	schedules: '<circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15 14"/>',
	workflows: '<circle cx="5" cy="6" r="2.4"/><circle cx="19" cy="6" r="2.4"/><circle cx="12" cy="18" r="2.4"/><path d="M6.7 7.6 10.6 16M17.3 7.6 13.4 16"/>',
	migrate: '<path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>',
	credentials: '<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
	users: '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
	audit: '<path d="M9 5H7a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-2"/><rect x="9" y="3" width="6" height="4" rx="1"/><path d="m9 14 2 2 4-4"/>',
	policies: '<path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z"/><polyline points="9 12 11 14 15 10"/>',
	doctor: '<path d="M14.7 6.3a4.8 4.8 0 0 0-6.4 6.4L3 18l3 3 5.3-5.3a4.8 4.8 0 0 0 6.4-6.4l-3.1 3.1-3-3z"/>',
	docs: '<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>',
};

// mountTopbar adds docs and repository links to the top bar on every page, so the guides and the
// source are one click away from anywhere in the product.
function mountTopbar() {
	const bar = document.querySelector(".topbar");
	if (!bar || bar.querySelector(".topbar-links")) return;
	if (document.body.dataset.page !== "login" && !bar.querySelector(".search-btn")) {
		const search = document.createElement("button");
		search.type = "button";
		search.className = "search-btn";
		const mac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
		search.innerHTML = svgIcon('<circle cx="11" cy="11" r="7"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>') +
			"<span>Search</span>" + '<span class="kbd">' + (mac ? "⌘K" : "Ctrl K") + "</span>";
		search.setAttribute("aria-label", "Search pages and actions");
		search.setAttribute("aria-haspopup", "dialog");
		search.addEventListener("click", openPalette);
		const brand = bar.querySelector(".brand");
		if (brand) brand.after(search); else bar.appendChild(search);
	}
	const nav = document.createElement("nav");
	nav.className = "topbar-links";
	if (document.body.dataset.page !== "login") {
		const tourWrap = document.createElement("div");
		tourWrap.className = "tour-launch";
		const tour = document.createElement("button");
		tour.type = "button";
		tour.className = "topbar-link tour-start";
		tour.textContent = "Tour";
		tour.setAttribute("aria-haspopup", "true");
		tour.setAttribute("aria-expanded", "false");
		tour.addEventListener("click", (e) => { e.stopPropagation(); toggleTourMenu(tour, tourWrap); });
		tourWrap.appendChild(tour);
		nav.appendChild(tourWrap);
	}
	const docs = document.createElement("a");
	docs.href = "/ui/docs";
	docs.className = "topbar-link";
	docs.textContent = "Docs";
	nav.appendChild(docs);
	const gh = document.createElement("a");
	gh.href = "https://github.com/kordloom/switchtender";
	gh.className = "topbar-link topbar-icon";
	gh.target = "_blank";
	gh.rel = "noopener";
	gh.title = "View on GitHub";
	gh.setAttribute("aria-label", "View on GitHub");
	gh.innerHTML = '<svg viewBox="0 0 16 16" width="18" height="18" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.02-1.49-2.01.44-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 012-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>';
	nav.appendChild(gh);
	bar.appendChild(nav);
}

// EXPORT_PAGES are the pages whose main table gets CSV and JSON export of the shown rows.
//
// Credentials are included on purpose: the API never returns a secret value, so an export lists
// names, kinds, secret state, and what uses them, which is what an access review needs. An earlier
// line here said the opposite, that credentials stayed out so secret-adjacent data could not leave
// by accident, and it sat directly above a list containing them. Whichever was written first, a
// comment that contradicts itself is worse than no comment: a reader cannot tell which half is the
// rule and which is the leftover.
const EXPORT_PAGES = ["runs", "fleet", "drift", "tasks", "compare", "workers", "schedules", "jobtemplates",
	"users", "audit", "host", "projects", "inventories", "sources", "policies", "doctor",
	"credentials"];

// tableRowsData reads the rendered table into headers and rows, skipping the actions column and
// anything hidden, so an export matches exactly what the user sees after filtering.
function tableRowsData(table) {
	const ths = Array.from(table.tHead.rows[0].cells);
	const skip = new Set();
	const headers = [];
	ths.forEach((th, i) => {
		const label = th.textContent.trim();
		if (th.classList.contains("col-actions") || label === "Actions" || label === "") skip.add(i);
		else headers.push(label);
	});
	const rows = [];
	for (const tr of table.tBodies[0].rows) {
		// Both filters are honored, and the pager deliberately is not. The text box and the facet
		// panel are the reader saying which rows they mean, so a file that carries rows they ticked
		// away is not the thing the button offered: picking one tool out of the facet panel and
		// exporting handed back every tool, which is worse than useless in an audit because it
		// looks like the answer to the question that was asked. The pager is a different kind of
		// thing, a limit on what fits the screen rather than on what was asked for, so the export
		// runs past it on purpose and the file holds every matching row instead of the first page.
		if (tr.dataset.fhide === "1" || tr.dataset.xhide === "1") continue;
		if (tr.classList.contains("skeleton-row")) continue;
		const row = [];
		Array.from(tr.cells).forEach((cell, i) => {
			if (skip.has(i)) return;
			// A cell that shows a truncated or graphical value carries the exportable one in
			// data-export, so the file gets the fact and not the abbreviation drawn from it.
			const exact = cell.dataset && cell.dataset.export;
			row.push(exact !== undefined && exact !== ""
				? exact
				: cell.textContent.replace(/\s+/g, " ").trim());
		});
		rows.push(row);
	}
	return { headers, rows };
}

// yamlScalar renders one value as YAML, quoting whenever the plain form would be ambiguous:
// empty strings, leading or trailing space, YAML indicators, and anything that would otherwise
// parse as a number, boolean, or null.
function yamlScalar(value) {
	if (value === null || value === undefined) return "null";
	if (typeof value === "number" || typeof value === "boolean") return String(value);
	const text = String(value);
	const risky = text === "" || text !== text.trim() ||
		/^[-?:,[\]{}#&*!|>'"%@`]/.test(text) || /:\s|\s#/.test(text) ||
		/^(true|false|null|yes|no|on|off|~)$/i.test(text) ||
		/^-?\d+(\.\d+)?$/.test(text) || text.includes("\n");
	if (!risky) return text;
	return '"' + text.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n") + '"';
}

// toYAML renders a list of flat records, or a single map, as a YAML document.
function toYAML(value, indent) {
	const pad = indent || "";
	if (Array.isArray(value)) {
		if (!value.length) return pad + "[]\n";
		return value.map((item) => {
			if (item && typeof item === "object") {
				const keys = Object.keys(item);
				if (!keys.length) return pad + "- {}\n";
				return keys.map((k, i) =>
					pad + (i === 0 ? "- " : "  ") + k + ": " + yamlScalar(item[k]) + "\n").join("");
			}
			return pad + "- " + yamlScalar(item) + "\n";
		}).join("");
	}
	if (value && typeof value === "object") {
		return Object.keys(value).map((k) => {
			const v = value[k];
			if (v && typeof v === "object") return pad + k + ":\n" + toYAML(v, pad + "  ");
			return pad + k + ": " + yamlScalar(v) + "\n";
		}).join("");
	}
	return pad + yamlScalar(value) + "\n";
}

// downloadBlob hands the browser a generated file.
function downloadBlob(name, type, content) {
	const url = URL.createObjectURL(new Blob([content], { type }));
	const a = document.createElement("a");
	a.href = url;
	a.download = name;
	document.body.appendChild(a);
	a.click();
	a.remove();
	URL.revokeObjectURL(url);
}

// CSV_FORMULA matches the leading characters a spreadsheet reads as the start of a formula rather
// than as text.
const CSV_FORMULA = /^[=+\-@\t\r]/;

// csvCell renders one value as a CSV field. A value that would open a formula is prefixed with a
// single quote, which every spreadsheet treats as "this is text". Exported cells carry names that
// came from outside, host names out of an imported inventory above all, so without the prefix an
// attacker who controls a host name gets code execution in whoever opens the export.
function csvCell(v) {
	let s = v === undefined || v === null ? "" : String(v);
	if (CSV_FORMULA.test(s)) s = "'" + s;
	return /[",\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s;
}

// mountTableExport adds CSV and JSON export buttons beside the list filter, so any table can
// leave the app for an audit, a spreadsheet, or a colleague. Every table on the page gets its
// own set: the compare page's task-timing table used to sit beside an exported table with no
// export of its own, which reads as an oversight because it was one.
function mountTableExport() {
	const page = document.body.dataset.page;
	if (!EXPORT_PAGES.includes(page)) return;
	const tables = Array.from(document.querySelectorAll("main.content table"))
		.filter((t) => t.tHead && t.tBodies[0]);
	tables.forEach((table, index) => mountExportsForTable(page, table, index));
}

// mountExportsForTable mounts one table's export buttons. The page's first table joins the
// existing toolbar or filter row; each later table gets its own row, and its files carry a
// numbered name so two tables from one page do not collide.
function mountExportsForTable(page, table, index) {
	// On the runs page the toolbar holds the filter and the dropdowns, so the export buttons join
	// the toolbar itself and land after them rather than beside the search box.
	let host = index === 0
		? (document.querySelector(".runs-toolbar") || document.querySelector(".list-filter"))
		: null;
	if (!host) {
		host = document.createElement("div");
		host.className = "list-filter";
		const anchor = table.closest(".list-scroll") || table;
		anchor.parentNode.insertBefore(host, anchor);
	}
	const filePart = page + (index > 0 ? "-" + (index + 1) : "");
	const stamp = () => new Date().toISOString().slice(0, 10);
	// prepare completes the table before it is read. The runs page pages from the server, so its
	// preparer pulls the rest of the current query in; every other page already renders whole.
	const prepare = async () => {
		if (page === "runs" && index === 0 && typeof runsExportPrepare === "function") {
			return (await runsExportPrepare()) || "";
		}
		return "";
	};
	const make = (label, tip, fn) => {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "button table-export";
		btn.innerHTML = svgIcon('<path d="M12 3v12"/><polyline points="7 10 12 15 17 10"/><path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/>');
		btn.appendChild(document.createTextNode(label));
		btn.dataset.tip = tip;
		// A page that knows its table is only part of the data turns these off, so the handler
		// checks rather than trusting the browser to withhold the click.
		btn.addEventListener("click", () => { if (!btn.disabled) fn(); });
		host.appendChild(btn);
	};
	make("CSV", "Click to export the filtered rows as a CSV spreadsheet", async () => {
		const suffix = await prepare();
		const { headers, rows } = tableRowsData(table);
		const csv = [headers, ...rows].map((r) => r.map(csvCell).join(",")).join("\n") + "\n";
		downloadBlob("switchtender-" + filePart + "-" + stamp() + suffix + ".csv", "text/csv", csv);
	});
	make("JSON", "Click to export the filtered rows as JSON", async () => {
		const suffix = await prepare();
		const { headers, rows } = tableRowsData(table);
		const objs = rows.map((r) => Object.fromEntries(headers.map((h, i) => [h, r[i] ?? ""])));
		downloadBlob("switchtender-" + filePart + "-" + stamp() + suffix + ".json", "application/json",
			JSON.stringify(objs, null, 2) + "\n");
	});
	make("YAML", "Click to export the filtered rows as YAML", async () => {
		const suffix = await prepare();
		const { headers, rows } = tableRowsData(table);
		const objs = rows.map((r) => Object.fromEntries(headers.map((h, i) => [h, r[i] ?? ""])));
		downloadBlob("switchtender-" + filePart + "-" + stamp() + suffix + ".yaml", "text/yaml", toYAML(objs));
	});
}

// SORT_SKIP names headers that hold controls rather than comparable values.
const SORT_SKIP = new Set(["", "actions", "fix", "recent", "history"]);

// cellSortValue reads a cell for comparison: a timestamp when the cell carries one, a number when
// the text is numeric, and lowercased text otherwise, so each column sorts the way it reads.
function cellSortValue(cell) {
	const timed = cell.querySelector("[data-time]") || (cell.dataset && cell.dataset.time ? cell : null);
	if (timed) {
		const t = Date.parse(timed.dataset.time);
		if (!isNaN(t)) return { n: t };
	}
	const text = cell.textContent.trim();
	// A leading number covers counts, durations, sizes, and ratios such as 1 / 10.
	const num = text.match(/^-?[\d,]+(\.\d+)?/);
	if (num && num[0].length >= text.replace(/[^\d.,\-].*$/, "").length && num[0] !== "") {
		const parsed = parseFloat(num[0].replace(/,/g, ""));
		if (!isNaN(parsed)) return { n: parsed };
	}
	return { s: text.toLowerCase() };
}

// mountTableSort makes every meaningful column header a sort control. Clicking cycles ascending
// then descending, and the active column shows its direction.
function mountTableSort() {
	const table = document.querySelector("main.content table");
	if (!table || !table.tHead || !table.tBodies[0]) return;
	const tbody = table.tBodies[0];
	Array.from(table.tHead.rows[0].cells).forEach((th, index) => {
		const label = th.textContent.trim().toLowerCase();
		if (SORT_SKIP.has(label) || th.classList.contains("col-actions")) return;
		th.classList.add("sortable");
		th.tabIndex = 0;
		th.setAttribute("role", "button");
		th.dataset.tip = "Click to sort by " + (th.textContent.trim() || "this column");
		const sort = () => {
			const desc = th.dataset.dir === "asc";
			for (const other of table.tHead.rows[0].cells) {
				if (other !== th) delete other.dataset.dir;
			}
			th.dataset.dir = desc ? "desc" : "asc";
			const rows = Array.from(tbody.rows).filter((r) => !r.classList.contains("skeleton-row"));
			rows.sort((a, b) => {
				const av = cellSortValue(a.cells[index]);
				const bv = cellSortValue(b.cells[index]);
				let cmp;
				if (av.n !== undefined && bv.n !== undefined) cmp = av.n - bv.n;
				else cmp = String(av.s ?? av.n).localeCompare(String(bv.s ?? bv.n));
				return desc ? -cmp : cmp;
			});
			for (const row of rows) tbody.appendChild(row);
			// Row numbering, where a table has it, follows the new order.
			let n = 0;
			for (const row of rows) {
				const numCell = row.querySelector("td.col-num");
				if (numCell) numCell.textContent = String(++n);
			}
			table.dispatchEvent(new CustomEvent("rowsfiltered"));
		};
		th.addEventListener("click", sort);
		th.addEventListener("keydown", (e) => {
			if (e.key === "Enter" || e.key === " ") { e.preventDefault(); sort(); }
		});
	});
}

// applyRowVisibility shows a row only when none of the text filter, the facets, or the pager hides
// it, so the three compose instead of fighting over the same flag.
function applyRowVisibility(row) {
	row.hidden = row.dataset.fhide === "1" || row.dataset.xhide === "1" || row.dataset.phide === "1";
}

// FACET_COLUMNS names the categorical column of each list that is worth checking off rather than
// typing at: the tool a template runs, what a credential holds, the role an account carries. The
// runs page is absent because it filters on the server, across every run rather than the loaded page.
const FACET_COLUMNS = {
	jobtemplates: ["Type"],
	credentials: ["Kind", "Source", "Secret"],
	users: ["Role"],
	schedules: ["Enabled"],
	fleet: ["Last outcome"],
	drift: ["State"],
	workers: ["Health"],
	sources: ["Status"],
	inventories: ["Format"],
	policies: ["Tool", "Holding"],
	audit: ["Method"],
	doctor: ["Severity"],
};

// CHIP_SELECTOR matches the small labels a cell shows instead of plain text. A cell built from chips
// is really a set of values, so each chip is faceted separately and a template tagged both TERRAFORM
// and DRY is found under either.
const CHIP_SELECTOR = ".tool-badge, .run-kind, .chip, .badge, .origin-chip, .cred-kind";

// cellFacetValues reads the values a cell contributes to a facet: one per chip when it holds chips,
// otherwise its text as a single value. A blank cell contributes nothing, so an empty column never
// grows a meaningless checkbox.
function cellFacetValues(cell) {
	if (!cell) return [];
	const chips = cell.querySelectorAll(CHIP_SELECTOR);
	if (chips.length) {
		return Array.from(chips).map((c) => c.textContent.trim()).filter(Boolean);
	}
	const text = cell.textContent.trim();
	return text && text !== "—" ? [text] : [];
}

// mountFacetFilters adds a checkbox menu per categorical column beside the list filter, so a list is
// narrowed by ticking the types wanted rather than by typing one of them. Values are discovered from
// the rendered rows, so a column gains a checkbox the moment a row uses it and the control needs no
// per-page vocabulary to maintain.
function mountFacetFilters() {
	const columns = FACET_COLUMNS[document.body.dataset.page];
	const wrap = document.querySelector(".list-filter");
	const table = document.querySelector("main.content table");
	if (!columns || !wrap || !table || !table.tHead || !table.tBodies[0]) return;
	const tbody = table.tBodies[0];
	const headers = Array.from(table.tHead.rows[0].cells).map((th) => th.textContent.trim().toLowerCase());
	const facets = [];
	for (const name of columns) {
		const index = headers.indexOf(name.toLowerCase());
		if (index !== -1) facets.push(mountFacet(wrap, table, tbody, name, index, () => applyFacets(facets, tbody, table)));
	}
	if (!facets.length) return;
	// Rows arrive after the mount on every page, and a sort reorders them, so the value lists and the
	// counts are rebuilt whenever the body changes.
	const refresh = () => {
		for (const f of facets) f.refresh();
		applyFacets(facets, tbody, table);
	};
	new MutationObserver(refresh).observe(tbody, { childList: true });
	refresh();
}

// applyFacets hides any row that fails a facet, leaving the text filter's and the pager's own flags
// alone, then tells the table its visible set changed.
function applyFacets(facets, tbody, table) {
	const active = facets.filter((f) => f.selected.size > 0);
	for (const row of tbody.rows) {
		if (row.classList.contains("skeleton-row")) continue;
		const pass = active.every((f) =>
			cellFacetValues(row.cells[f.index]).some((v) => f.selected.has(v)));
		row.dataset.xhide = pass ? "" : "1";
		applyRowVisibility(row);
	}
	table.dispatchEvent(new CustomEvent("rowsfiltered"));
}

// mountFacet builds one column's checkbox menu: a labeled button that opens a panel of the values
// present in that column, each with how many rows carry it. It returns the facet's state so the
// mount can rebuild its values and apply the whole set together.
function mountFacet(wrap, table, tbody, name, index, onChange) {
	const facet = { name, index, selected: new Set(), refresh: null };
	const host = document.createElement("div");
	host.className = "facet";
	const button = document.createElement("button");
	button.type = "button";
	button.className = "button facet-btn";
	button.setAttribute("aria-expanded", "false");
	button.dataset.tip = "Click to filter this list by " + name.toLowerCase();
	const label = document.createElement("span");
	label.textContent = name;
	const count = document.createElement("span");
	count.className = "facet-count";
	count.hidden = true;
	button.appendChild(label);
	button.appendChild(count);
	button.insertAdjacentHTML("beforeend",
		'<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" ' +
		'stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
		'<polyline points="6 9 12 15 18 9"/></svg>');
	const panel = document.createElement("div");
	panel.className = "facet-panel";
	panel.hidden = true;
	panel.setAttribute("role", "group");
	panel.setAttribute("aria-label", name + " filter");
	host.appendChild(button);
	host.appendChild(panel);
	wrap.appendChild(host);

	const syncButton = () => {
		count.textContent = String(facet.selected.size);
		count.hidden = facet.selected.size === 0;
		host.classList.toggle("facet-on", facet.selected.size > 0);
	};
	// refresh rebuilds the value list from the rows that are in the table now, keeping any tick whose
	// value is still present and dropping the control entirely for a column with nothing to choose.
	facet.refresh = () => {
		const counts = new Map();
		for (const row of tbody.rows) {
			if (row.classList.contains("skeleton-row")) continue;
			for (const v of cellFacetValues(row.cells[index])) {
				counts.set(v, (counts.get(v) || 0) + 1);
			}
		}
		for (const v of Array.from(facet.selected)) {
			if (!counts.has(v)) facet.selected.delete(v);
		}
		host.hidden = counts.size < 2;
		panel.textContent = "";
		const values = Array.from(counts.keys()).sort((a, b) => a.localeCompare(b));
		for (const value of values) {
			const row = document.createElement("label");
			row.className = "facet-item";
			const box = document.createElement("input");
			box.type = "checkbox";
			box.checked = facet.selected.has(value);
			box.addEventListener("change", () => {
				if (box.checked) facet.selected.add(value);
				else facet.selected.delete(value);
				syncButton();
				onChange();
			});
			const text = document.createElement("span");
			text.className = "facet-item-label";
			text.textContent = value;
			const n = document.createElement("span");
			n.className = "facet-item-count";
			n.textContent = String(counts.get(value));
			row.appendChild(box);
			row.appendChild(text);
			row.appendChild(n);
			panel.appendChild(row);
		}
		const clear = document.createElement("button");
		clear.type = "button";
		clear.className = "facet-clear";
		clear.textContent = "Clear";
		clear.addEventListener("click", () => {
			facet.selected.clear();
			for (const box of panel.querySelectorAll("input")) box.checked = false;
			syncButton();
			onChange();
		});
		panel.appendChild(clear);
		syncButton();
	};

	const setOpen = (open) => {
		panel.hidden = !open;
		button.setAttribute("aria-expanded", open ? "true" : "false");
	};
	button.addEventListener("click", (e) => {
		e.stopPropagation();
		const opening = panel.hidden;
		for (const other of document.querySelectorAll(".facet-panel")) other.hidden = true;
		for (const other of document.querySelectorAll(".facet-btn")) {
			other.setAttribute("aria-expanded", "false");
		}
		setOpen(opening);
	});
	panel.addEventListener("click", (e) => e.stopPropagation());
	document.addEventListener("click", () => setOpen(false));
	host.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !panel.hidden) { setOpen(false); button.focus(); }
	});
	return facet;
}

// mountTablePager caps how many rows a list shows at once, with a footer for paging: a count, a
// rows-per-page choice, and Show all. Long fleets and logs stay wieldy, and the filter composes,
// since it marks rows and the pager only pages the ones that match. The runs page pages on the
// server and is left alone.
function mountTablePager() {
	const page = document.body.dataset.page;
	if (page === "runs" || !EXPORT_PAGES.includes(page)) return;
	const table = document.querySelector("main.content table");
	if (!table || !table.tBodies[0]) return;
	const tbody = table.tBodies[0];
	const foot = document.createElement("div");
	foot.className = "table-foot";
	foot.hidden = true;
	const footAnchor = table.closest(".list-scroll") || table;
	footAnchor.parentNode.insertBefore(foot, footAnchor.nextSibling);
	const count = document.createElement("span");
	const spacer = document.createElement("span");
	spacer.className = "spacer";
	const label = document.createElement("label");
	label.className = "pagesize-label";
	label.textContent = "Rows";
	const sel = document.createElement("select");
	sel.className = "input toolbar-select";
	for (const n of [25, 50, 100, 0]) {
		const opt = document.createElement("option");
		opt.value = String(n);
		opt.textContent = n === 0 ? "All" : String(n);
		sel.appendChild(opt);
	}
	label.appendChild(sel);
	const all = document.createElement("button");
	all.type = "button";
	all.className = "button";
	all.textContent = "Show all";
	foot.appendChild(count);
	foot.appendChild(spacer);
	foot.appendChild(label);
	foot.appendChild(all);
	let size = 25;
	const apply = () => {
		let shown = 0;
		let matched = 0;
		for (const row of tbody.rows) {
			if (row.classList.contains("skeleton-row")) continue;
			// Both filters have to be honored here. Counting a facet-hidden row as a match let it
			// consume a page slot, so ticking a facet value whose rows all sat past the page size
			// emptied the table while the facet panel still reported matches: the filter read as
			// broken, and a reviewer concluded there were no such entries.
			if (row.dataset.fhide === "1" || row.dataset.xhide === "1") {
				row.dataset.phide = "";
				applyRowVisibility(row);
				continue;
			}
			matched++;
			row.dataset.phide = size && matched > size ? "1" : "";
			applyRowVisibility(row);
			if (!row.hidden) shown++;
		}
		foot.hidden = matched <= 25 && size >= 25;
		count.textContent = "Showing " + shown + " of " + matched;
		all.hidden = shown >= matched;
	};
	sel.addEventListener("change", () => { size = parseInt(sel.value, 10) || 0; apply(); });
	all.addEventListener("click", () => { size = 0; sel.value = "0"; apply(); });
	table.addEventListener("rowsfiltered", apply);
	new MutationObserver(apply).observe(tbody, { childList: true });
	apply();
}

