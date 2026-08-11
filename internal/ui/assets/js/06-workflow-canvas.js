// startPinch records the two-finger baseline so a pinch scales from where the fingers began.
function startPinch() {
	const pts = [...wfState.pointers.values()];
	wfState.pinch = {
		dist: ptDist(pts[0], pts[1]),
		cx: (pts[0].x + pts[1].x) / 2, cy: (pts[0].y + pts[1].y) / 2,
		scale: wfState.scale, panX: wfState.panX, panY: wfState.panY,
	};
}

// ptDist is the distance between two pointer positions.
function ptDist(a, b) {
	return Math.hypot(a.x - b.x, a.y - b.y);
}

// wfWheel pans on a plain scroll and zooms toward the cursor when a modifier or a trackpad pinch is
// held, so a mouse wheel and a trackpad both feel right.
function wfWheel(e) {
	e.preventDefault();
	if (e.ctrlKey || e.metaKey) {
		zoomAt(e.clientX, e.clientY, wfState.scale * Math.exp(-e.deltaY * 0.0015));
		return;
	}
	wfState.panX -= e.deltaX;
	wfState.panY -= e.deltaY;
	applyViewport();
	saveViewSoon();
}

// wfPointerMove updates a pinch, a pan, an in-progress node drag, or a link as the pointer moves. A
// drag starts only past a small threshold, so a shaky tap still opens the editor, and only the
// dragged card is repositioned so large graphs stay smooth.
function wfPointerMove(e) {
	if (wfState.pointers.has(e.pointerId)) {
		wfState.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
	}
	if (wfState.pinch && wfState.pointers.size >= 2) {
		const pts = [...wfState.pointers.values()];
		const rect = wfState.canvas.getBoundingClientRect();
		const k = clampScale(wfState.pinch.scale * (ptDist(pts[0], pts[1]) / wfState.pinch.dist));
		const wx = (wfState.pinch.cx - rect.left - wfState.pinch.panX) / wfState.pinch.scale;
		const wy = (wfState.pinch.cy - rect.top - wfState.pinch.panY) / wfState.pinch.scale;
		wfState.scale = k;
		wfState.panX = (pts[0].x + pts[1].x) / 2 - rect.left - wx * k;
		wfState.panY = (pts[0].y + pts[1].y) / 2 - rect.top - wy * k;
		applyViewport();
		saveViewSoon();
		return;
	}
	if (wfState.pan) {
		wfState.panX = wfState.pan.px + (e.clientX - wfState.pan.x);
		wfState.panY = wfState.pan.py + (e.clientY - wfState.pan.y);
		applyViewport();
		saveViewSoon();
		return;
	}
	if (wfState.drag) {
		const p = wfPoint(e);
		if (!wfState.drag.moved &&
			Math.abs(p.x - wfState.drag.sx) < 4 && Math.abs(p.y - wfState.drag.sy) < 4) {
			return;
		}
		wfState.drag.moved = true;
		const node = wfState.nodes.find((n) => n.id === wfState.drag.id);
		node.x = Math.max(0, p.x - wfState.drag.dx);
		node.y = Math.max(0, p.y - wfState.drag.dy);
		positionNode(node.id);
		renderEdges();
	} else if (wfState.link) {
		wfState.link.cursor = wfPoint(e);
		renderEdges();
	}
}

// wfPointerUp ends a pan or pinch, finishes a drag, or completes a link when it lands on a node.
function wfPointerUp(e) {
	wfState.pointers.delete(e.pointerId);
	if (wfState.pinch && wfState.pointers.size < 2) {
		wfState.pinch = null;
		wfSave();
	}
	if (wfState.pan) {
		wfState.pan = null;
		wfState.canvas.classList.remove("wf-panning");
		wfSave();
	}
	if (wfState.link) {
		// Pointer capture retargets the event to the canvas, so find the drop node by geometry.
		const el = document.elementFromPoint(e.clientX, e.clientY);
		const over = el ? el.closest(".wf-node") : null;
		const fromId = wfState.link.from;
		wfState.link = null;
		if (over) linkTo(fromId, over.dataset.id);
		renderEdges();
	}
	if (wfState.drag) {
		if (wfState.drag.moved) {
			wfPushHistory(wfState.drag.before);
			wfSave();
		} else {
			openStepModal(wfState.nodes.find((n) => n.id === wfState.drag.id));
		}
		wfState.drag = null;
	}
}

// wfCancelPointer clears a pan, pinch, drag, or link whose gesture was canceled, for example by an
// edge swipe on a touch screen, so nothing stays glued to the pointer.
function wfCancelPointer(e) {
	wfState.pointers.delete(e.pointerId);
	if (wfState.pinch && wfState.pointers.size < 2) wfState.pinch = null;
	if (wfState.pan) {
		wfState.pan = null;
		wfState.canvas.classList.remove("wf-panning");
	}
	if (wfState.drag) {
		if (wfState.drag.moved) {
			wfPushHistory(wfState.drag.before);
			wfSave();
		}
		wfState.drag = null;
	}
	if (wfState.link) {
		wfState.link = null;
		renderEdges();
	}
}

// linkTo adds a dependency edge from one node to another, rejecting self-links, duplicates, and
// edges that would create a cycle. The status line announces the new dependency.
function linkTo(fromId, toId) {
	if (fromId === toId) return;
	if (wfState.edges.some((e) => e.from === fromId && e.to === toId)) return;
	if (reaches(toId, fromId)) {
		wfSetStatus("That link would create a cycle.", "err");
		return;
	}
	const from = wfState.nodes.find((n) => n.id === fromId);
	const to = wfState.nodes.find((n) => n.id === toId);
	if (!from || !to) return;
	wfSnapshot();
	wfState.edges.push({ from: fromId, to: toId });
	wfSetStatus(to.name + " now waits for " + from.name + ".", "");
	wfSave();
}

// reaches reports whether following edges from start eventually arrives at goal, used to block
// cycles before they are added.
function reaches(start, goal) {
	const seen = new Set();
	const stack = [start];
	while (stack.length) {
		const cur = stack.pop();
		if (cur === goal) return true;
		if (seen.has(cur)) continue;
		seen.add(cur);
		for (const e of wfState.edges) {
			if (e.from === cur) stack.push(e.to);
		}
	}
	return false;
}

// wfSetStatus writes the editor status line, red for an error.
function wfSetStatus(msg, kind) {
	const el = document.getElementById("status");
	if (!el) return;
	el.className = kind === "err" ? "error-text" : "muted";
	el.textContent = msg;
	el.hidden = !msg;
}

// saveWorkflowTemplate stores the graph as a reusable workflow template rather than firing it once.
//
// A graph built here used to be fire-and-forget: it ran as a pipeline and was gone. Saving it makes
// it an object a schedule, a webhook, or a later launch can fire, which is what a workflow is for.
// The template carries only the graph, so it sets no top-level tool or playbook, which is what the
// API requires of a workflow template.
async function saveWorkflowTemplate() {
	if (wfState.submitting) return;
	if (wfState.nodes.length === 0) {
		wfSetStatus("Add at least one step before saving.", "err");
		return;
	}
	const doc = workflowDocument();
	const body = { name: doc.name, steps: doc.steps };
	if (doc.inventory) body.inventory = doc.inventory;
	const btn = document.getElementById("wf-save-template");
	wfState.submitting = true;
	if (btn) btn.disabled = true;
	wfSetStatus("Saving workflow template.", "");
	try {
		const res = await fetch(API + "/templates", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify(body),
		});
		if (res.status === 401) {
			wfSave();
			requireLogin();
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) {
			wfSetStatus(data.error || ("Could not save the template: HTTP " + res.status), "err");
			return;
		}
		wfSetStatus("Saved as the workflow template " + (data.name || doc.name) +
			". A schedule or a webhook can fire it now.", "ok");
	} catch (err) {
		wfSetStatus("Could not save the template: " + err.message, "err");
	} finally {
		wfState.submitting = false;
		if (btn) btn.disabled = false;
	}
}

// runWorkflow serializes the graph into pipeline steps and submits it, then opens the new run. An
// in-flight guard stops a double click from starting the workflow twice, and a 401 saves the draft
// and routes through sign-in so the graph survives the round trip.
// workflowSteps renders the canvas graph into the pipeline steps the API accepts, resolving each
// edge into a dependency by step name.
function workflowSteps() {
	return wfState.nodes.map((n) => {
		const step = { name: n.name, tool: n.tool };
		if (n.tool === "ansible") step.playbook = n.playbook;
		else step.command = n.command;
		if (n.inventory) step.inventory = n.inventory;
		if (n.dryRun) step.dry_run = true;
		if (n.continueOnFailure) step.continue_on_failure = true;
		if (n.retries > 0) step.retries = n.retries;
		const deps = wfState.edges.filter((e) => e.to === n.id)
			.map((e) => {
				const src = wfState.nodes.find((x) => x.id === e.from);
				return src ? src.name : null;
			})
			.filter(Boolean);
		if (deps.length) step.depends_on = deps;
		return step;
	});
}

// workflowDocument is the whole pipeline as it would be submitted, for running or exporting.
function workflowDocument() {
	return {
		name: document.getElementById("wf-name").value.trim() || "workflow",
		inventory: document.getElementById("wf-inventory").value.trim(),
		steps: workflowSteps(),
	};
}

// exportWorkflow downloads the graph as a pipeline definition, so a workflow built visually can
// be committed to a repository or handed to the API.
function exportWorkflow(format) {
	if (!wfState || !wfState.nodes.length) {
		wfSetStatus("Add at least one step before exporting.", "err");
		return;
	}
	const doc = workflowDocument();
	const name = (doc.name || "workflow").replace(/\s+/g, "-").toLowerCase();
	if (format === "yaml") {
		downloadBlob(name + ".yaml", "text/yaml", toYAML(doc));
	} else {
		downloadBlob(name + ".json", "application/json", JSON.stringify(doc, null, 2) + "\n");
	}
	wfSetStatus("Exported " + doc.steps.length + " steps.", "");
}

async function runWorkflow() {
	if (wfState.submitting) return;
	if (wfState.nodes.length === 0) { wfSetStatus("Add at least one step.", "err"); return; }
	const steps = workflowSteps();
	const body = {
		name: document.getElementById("wf-name").value.trim() || "workflow",
		inventory: document.getElementById("wf-inventory").value.trim(),
		steps,
	};
	const approval = document.getElementById("wf-require-approval");
	if (approval && approval.checked) body.require_approval = true;
	const runBtn = document.getElementById("wf-run");
	wfState.submitting = true;
	runBtn.disabled = true;
	wfSetStatus("Starting workflow.", "");
	try {
		const res = await fetch(API + "/pipelines", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify(body),
		});
		if (res.status === 401) {
			wfSave();
			requireLogin();
			return;
		}
		if (!res.ok) {
			const detail = await res.json().catch(() => ({}));
			throw new Error(detail.error || "HTTP " + res.status);
		}
		const run = await res.json();
		try { localStorage.removeItem(wfDraftKey); } catch { /* draft already gone */ }
		window.location.assign("/ui/runs/" + run.id);
	} catch (err) {
		wfSetStatus("Could not start workflow: " + err.message, "err");
		wfState.submitting = false;
		runBtn.disabled = false;
	}
}

document.addEventListener("DOMContentLoaded", () => {
	// Any link that starts a sign-in marks this tab first, so the token that comes back can be
	// answered to a request this browser made.
	for (const el of document.querySelectorAll("[data-sso]")) {
		el.addEventListener("click", beginSSO);
	}
	consumeSSOFragment();
	mountTopbar();
	mountLiveRegions();
	explainReadOnly();
	if (LIST_PAGES.includes(document.body.dataset.page)) {
		mountListFilter();
		mountFacetFilters();
	}
	const close = document.getElementById("drill-close");
	if (close) {
		close.addEventListener("click", () => { document.getElementById("drill").hidden = true; });
	}
	const page = document.body.dataset.page;
	if (page === "overview") {
		loadOverview();
		wireAsk();
	} else if (page === "runs") {
		wireModal("launch");
		if (!isReadOnly()) wireLaunchForm();
		wirePropose();
		wireRunsSearch();
		wireRunsFilters();
		loadRuns();
	} else if (page === "detail") {
		loadDetail(document.body.dataset.runId);
	} else if (page === "fleet") {
		loadFleet();
	} else if (page === "drift") {
		loadDrift();
	} else if (page === "host") {
		loadHost(document.body.dataset.host);
	} else if (page === "tasks") {
		loadTasks();
	} else if (page === "schedules") {
		wireCronPreview();
		wireModal("schedule");
		wireScheduleForm();
		loadSchedules();
	} else if (page === "workflows") {
		mountWorkflow();
	} else if (page === "login") {
		loadLogin();
	} else if (page === "credentials") {
		wireModal("cred");
		wireCredentialForm();
		loadCredentials();
	} else if (page === "audit") {
		wireAudit();
		loadAudit();
	} else if (page === "policies") {
		wireModal("policy");
		wirePolicyForm();
		loadPolicies();
	} else if (page === "projects") {
		wireModal("project");
		wireProjectForm();
		loadProjects();
	} else if (page === "jobtemplates") {
		wireModal("template");
		wireTemplateForm();
		loadTemplates();
	} else if (page === "users") {
		wireModal("user");
		wireUserForm();
		loadUsers();
	} else if (page === "workers") {
		loadWorkers();
	} else if (page === "inventories") {
		wireModal("inventory");
		wireInventoryForm();
		loadInventories();
	} else if (page === "sources") {
		wireModal("source");
		wireSourceForm();
		loadSources();
	} else if (page === "migrate") {
		wireMigrate();
	} else if (page === "doctor") {
		loadDoctor();
	}
	if (page === "runs") mountRunsWindowChip();
	buildNav();
	mountFooter();
	if (document.body.dataset.page === "docs") mountDocsChrome();
	wirePalette();
	wireHinttips();
	mountPageDocs();
	mountTableExport();
	mountTablePager();
	mountTableSort();
	if (isReadOnly()) applyReadOnly();
	setInterval(refreshRelTimes, 20000);
	mountTour();
});

// showSkeletonRows fills a table body with shimmering placeholders while its data loads.
function showSkeletonRows(tbody, rows, cols) {
	tbody.innerHTML = "";
	for (let i = 0; i < rows; i++) {
		const tr = document.createElement("tr");
		tr.className = "skeleton-row";
		for (let c = 0; c < cols; c++) {
			const cell = document.createElement("td");
			const bar = document.createElement("span");
			bar.className = "skeleton";
			cell.appendChild(bar);
			tr.appendChild(cell);
		}
		tbody.appendChild(tr);
	}
}

// applyReadOnly keeps the forms and controls visible so the demo conveys what the product does, but
// disables the actions that would mutate and adds a banner. Row action buttons are dimmed by CSS.
function applyReadOnly() {
	const main = document.querySelector(".content");
	if (main && !main.querySelector(".ro-banner")) {
		const banner = document.createElement("div");
		banner.className = "ro-banner";
		banner.textContent = "Read-only demo. Browse the data freely. Changes are disabled.";
		main.insertBefore(banner, main.firstChild);
	}
	// Anything that would change state is disabled wherever it lives: rows, panels, drills, and
	// page headers alike, each explaining itself rather than silently doing nothing.
	// The page header's primary action creates something, so it is mutating by definition.
	for (const btn of document.querySelectorAll(".page-head .button.primary, .wf-toolbar .button.primary")) {
		btn.dataset.mutates = "true";
		if (!btn.dataset.tip) {
			btn.dataset.tip = "Click to " + btn.textContent.trim().toLowerCase();
		}
	}
	// Table rows and drill panels are built after this pass runs, so rather than disabling the
	// controls that exist right now, swallow the click for anything marked as mutating. The
	// control stays hoverable, which is what lets it explain why it is unavailable.
	document.addEventListener("click", (e) => {
		const target = e.target.closest && e.target.closest("[data-mutates]");
		if (!target) return;
		e.preventDefault();
		e.stopImmediatePropagation();
	}, true);
	for (const form of document.querySelectorAll("form")) {
		for (const btn of form.querySelectorAll("button")) btn.disabled = true;
		const actions = form.querySelector(".launch-actions") || form;
		if (!actions.querySelector(".ro-note")) {
			const note = document.createElement("span");
			note.className = "ro-note";
			note.textContent = "Disabled in the demo";
			actions.appendChild(note);
		}
	}
	// Building, dragging, zooming, and exporting a graph never touch the server, so the editor
	// stays fully usable in the demo. Only running the pipeline is blocked.
	const wfRun = document.getElementById("wf-run");
	if (wfRun) {
		wfRun.dataset.mutates = "true";
		wfRun.dataset.tip = "Click to run this graph as a pipeline";
	}
	const wfSaveBtn = document.getElementById("wf-save-template");
	if (wfSaveBtn) {
		wfSaveBtn.dataset.mutates = "true";
		wfSaveBtn.dataset.tip = "Click to save this graph as a reusable workflow template";
	}
}

