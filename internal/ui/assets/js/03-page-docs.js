// PAGE_DOCS maps each page to the guide that explains that page's subject. Every page but sign in
// has an entry, so the same help chip appears in the same place everywhere and becomes something a
// reader learns to look for.
const PAGE_DOCS = {
	overview: { slug: "quickstart", label: "Quickstart" },
	runs: { slug: "tutorial-run-a-job", label: "Run a job" },
	detail: { slug: "tutorial-run-a-job", label: "Run a job" },
	fleet: { slug: "reliability", label: "Reliability" },
	host: { slug: "drift", label: "Drift detection" },
	drift: { slug: "drift", label: "Drift detection" },
	tasks: { slug: "concepts", label: "Concepts" },
	workers: { slug: "reliability", label: "Reliability" },
	projects: { slug: "concepts", label: "Concepts" },
	inventories: { slug: "concepts", label: "Concepts" },
	sources: { slug: "concepts", label: "Concepts" },
	jobtemplates: { slug: "tutorial-save-a-template", label: "Save a template" },
	workflows: { slug: "concepts", label: "Concepts" },
	schedules: { slug: "tutorial-schedule-a-job", label: "Schedule a job" },
	migrate: { slug: "tutorial-migrate", label: "Migrate your setup" },
	credentials: { slug: "secrets", label: "Secrets" },
	users: { slug: "configuration", label: "Configuration" },
	audit: { slug: "features", label: "Features" },
	policies: { slug: "features", label: "Features" },
	doctor: { slug: "concepts", label: "Concepts" },
	docs: { slug: "", label: "All guides" },
};

// docsChip builds the page's help control: one book mark and the guide's name, in the same tinted
// pill on every page so it reads as help rather than as another action.
function docsChip(ref) {
	const a = document.createElement("a");
	a.className = "docs-link";
	a.href = "/ui/docs" + (ref.slug ? "/" + ref.slug : "");
	a.dataset.tip = ref.slug
		? "Open the " + ref.label + " guide, the documentation for this page"
		: "Open the guide index";
	a.innerHTML = svgIcon(NAV_ICONS.docs);
	a.appendChild(document.createTextNode(ref.label));
	return a;
}

// pageHeadActions returns the page header's action row, building the header when the page has none.
// A page that opens on a bare heading is promoted to the standard header so its help chip sits
// where every other page's does; a page with no heading at all gets a lone right-aligned row.
function pageHeadActions(main) {
	const head = main.querySelector(".page-head");
	if (head) {
		let actions = head.querySelector(".head-actions");
		if (!actions) {
			actions = document.createElement("div");
			actions.className = "head-actions";
			for (const child of Array.from(head.children)) {
				if (!child.classList.contains("page-head-text")) actions.appendChild(child);
			}
			head.appendChild(actions);
		}
		return actions;
	}
	const built = document.createElement("div");
	built.className = "page-head";
	const actions = document.createElement("div");
	actions.className = "head-actions";
	const h1 = main.querySelector(":scope > h1");
	if (h1) {
		const text = document.createElement("div");
		text.className = "page-head-text";
		const sub = h1.nextElementSibling;
		h1.replaceWith(built);
		text.appendChild(h1);
		if (sub && sub.tagName === "P" && sub.classList.contains("muted")) text.appendChild(sub);
		built.appendChild(text);
	} else {
		// No heading to promote, so the row stands alone at the top of the content, after the back
		// link when the page opens on one.
		built.classList.add("page-head-bare");
		const back = main.querySelector(":scope > .back");
		if (back) back.insertAdjacentElement("afterend", built);
		else main.insertBefore(built, main.firstChild);
	}
	built.appendChild(actions);
	return actions;
}

// mountPageDocs puts the guide chip on the page, so every screen points at the documentation for
// the subject it is showing.
function mountPageDocs() {
	const ref = PAGE_DOCS[document.body.dataset.page];
	const main = document.querySelector("main.content");
	if (!ref || !main) return;
	pageHeadActions(main).appendChild(docsChip(ref));
}

// LIST_PAGES are the pages whose main table is a searchable list.
// LIST_PAGES get the client-side row filter. The runs page is excluded because it searches on the
// server, across every run rather than only the loaded page.
const LIST_PAGES = ["jobtemplates", "credentials", "projects", "inventories", "sources",
	"schedules", "users", "workers", "fleet", "tasks", "host", "policies", "drift", "audit", "doctor"];

// mountListFilter adds a search box above the main list table and filters its rows by text as you
// type, so every list is searchable. It reads the rows live, so it works no matter when they load.
function mountListFilter() {
	const table = document.querySelector("main.content table");
	if (!table || !table.tBodies[0]) return;
	const tbody = table.tBodies[0];
	const wrap = document.createElement("div");
	wrap.className = "list-filter";
	const input = document.createElement("input");
	input.type = "search";
	input.className = "input list-filter-input";
	input.placeholder = "Filter this list…";
	input.setAttribute("aria-label", "Filter list");
	const count = document.createElement("span");
	count.className = "muted list-filter-count";
	wrap.appendChild(input);
	wrap.appendChild(count);
	table.parentNode.insertBefore(wrap, table);
	const preset = new URLSearchParams(location.search).get("q");
	// Filtering walks every row's text and then repaginates, so a burst of keystrokes is debounced
	// into one pass, the same way the runs search batches its requests.
	const filter = () => {
		const q = input.value.trim().toLowerCase();
		let shown = 0;
		for (const row of tbody.rows) {
			const match = q === "" || row.textContent.toLowerCase().includes(q);
			if (match) row.dataset.fhide = "";
			else row.dataset.fhide = "1";
			applyRowVisibility(row);
			if (match) shown++;
		}
		count.textContent = q ? shown + " shown" : "";
		table.dispatchEvent(new CustomEvent("rowsfiltered"));
	};
	let filterTimer;
	input.addEventListener("input", () => {
		clearTimeout(filterTimer);
		filterTimer = setTimeout(filter, 150);
	});
	if (preset) {
		input.value = preset;
		// Rows may not be loaded yet, so the preset re-applies as they arrive.
		new MutationObserver(() => input.dispatchEvent(new Event("input")))
			.observe(tbody, { childList: true });
		input.dispatchEvent(new Event("input"));
	}
}

// TOURS is the guided-tour registry. Each tour runs on one page and walks a sequence of steps; a
// step with a selector spotlights that element, and a step without one shows a centered card. The
// launcher in the top bar lists them, and the welcome tour also runs on a first visit.
const TOURS = [
	{
		id: "welcome", title: "Sixty-second tour", desc: "The whole product at a glance",
		page: "overview", path: "/ui/",
		steps: [
			{ title: "Welcome to SwitchTender", body: "One binary runs Ansible, Terraform, Bash, Python, and Go, with no Kubernetes. Here is the sixty-second tour." },
			{ sel: ".page-head .button.primary", title: "Launch any tool", body: "Start a run with Ansible, Bash, Terraform, or Python, each with a dry run, and mix them in a single pipeline." },
			{ sel: ".panel-runs", title: "Watch every run", body: "Runs stream live here, with a host matrix, sharded splits, and multi-step pipelines all in one place." },
			{ sel: "#tiles a[href='/ui/migrate']", title: "Bring your work with you", body: "Migrating from another tool? Import projects, inventories, templates, and schedules in a few clicks." },
			{ sel: ".tile-search", title: "Find anything fast", body: "This search filters instantly, and every list in SwitchTender is searchable the same way." },
			{ sel: ".side|.nav-toggle", title: "The rest of the yard", body: "Job templates, credentials with external secrets, schedules, and fleet analytics all live in the navigation." },
			{ title: "You are set", body: "Explore the demo freely. Nothing here can be broken. Replay this tour anytime from Tour in the top bar." },
		],
	},
	{
		id: "pitch", title: "Why teams switch", desc: "The sixty-second pitch, hands free",
		page: "overview", path: "/ui/", auto: true,
		steps: [
			{ title: "Run everything. Watch every host. Prove every change.", body: "Your whole automation stack in one binary, with no Kubernetes to stand up first. Sit back, this tour drives itself.", hold: 6000 },
			{ sel: ".panel-runs", title: "Watch every host", body: "Every run is a live host-by-task matrix, not a wall of scrollback. A failure shows the moment it happens, on the host it happened to.", hold: 7000 },
			{ sel: "#ask-panel", title: "Ask the fleet anything", body: "Advisory AI answers from run, health, and drift data. It proposes and never executes. Run it on local Ollama or your own cloud key.", hold: 7000 },
			{ page: "workflows", path: "/ui/workflows", sel: "#wf-canvas", title: "Drag a pipeline together", body: "Wire all seven tools, and any tool you plug in, into one graph with per-step retries. AWX's signature feature, without the Kubernetes bill.", hold: 7500 },
			{ page: "policies", path: "/ui/policies", sel: "#policy-open", title: "The gate nobody skips", body: "Policy holds a prod terraform destroy for an admin's sign-off, automatically. Approvals are enforced, not suggested.", hold: 7000 },
			{ page: "audit", path: "/ui/audit", sel: "#audit-verify", title: "Prove every change", body: "Every change links into a tamper-evident hash chain. One click verifies it here, and a signed bundle verifies offline with an open verifier.", hold: 7000 },
			{ page: "overview", path: "/ui/", sel: "#tiles a[href='/ui/migrate']", title: "Switching is one command", body: "Projects, inventories, templates, surveys, and schedules import from AWX or Semaphore in a single pass.", hold: 6500 },
			{ title: "That is the moat", body: "Running many tools is table stakes. A control plane that proves itself is not. Press Explore and try anything, nothing here can break.", hold: 8000 },
		],
	},
	{
		id: "migrate", title: "Coming from AWX", desc: "Move your automation over",
		page: "migrate", path: "/ui/migrate",
		steps: [
			{ title: "Leave AWX or Semaphore behind", body: "Import your projects, inventories, templates, surveys, and schedules in a single pass." },
			{ title: "Preview before you commit", body: "Every import runs as a dry run first, showing exactly what it will create. Apply it when it looks right." },
			{ title: "No lock-in", body: "You can export and leave anytime, too. SwitchTender earns the switch. It does not trap you." },
		],
	},
];

// tourByID returns the tour with the given id, or null when none matches.
function tourByID(id) {
	return TOURS.find((t) => t.id === id) || null;
}

