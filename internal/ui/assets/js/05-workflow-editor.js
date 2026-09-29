// wfDraftKey is the localStorage key holding the unsent workflow graph, so a refresh, a stray
// navigation, or a sign-in round trip does not lose the work.
const wfDraftKey = "st_wf_draft";

// wfHistoryCap bounds the undo stack.
const wfHistoryCap = 50;

// mountWorkflow initializes the visual workflow editor: the node graph, the canvas drag and link
// interactions, the keyboard bindings, and the toolbar that adds steps and runs the graph as a
// pipeline. Any saved draft is restored before first paint.
function mountWorkflow() {
	const canvas = document.getElementById("wf-canvas");
	if (!canvas) return;
	wfState = {
		nodes: [], edges: [], seq: 0, editing: null, canvas,
		world: canvas.querySelector(".wf-world"),
		nodesLayer: canvas.querySelector(".wf-nodes"),
		edgesLayer: canvas.querySelector(".wf-edges"),
		hint: canvas.querySelector(".wf-hint"),
		drag: null, link: null, linkFrom: null, selectedEdge: null,
		past: [], future: [], lastSnapKey: null, lastSnapAt: 0,
		opener: null, submitting: false,
		scale: 1, panX: 40, panY: 40, pan: null, pointers: new Map(), pinch: null,
	};
	document.getElementById("wf-add").addEventListener("click", () => openStepModal(null));
	document.getElementById("wf-run").addEventListener("click", runWorkflow);
	const wfJSON = document.getElementById("wf-export-json");
	if (wfJSON) wfJSON.addEventListener("click", () => exportWorkflow("json"));
	const wfYAML = document.getElementById("wf-export-yaml");
	if (wfYAML) wfYAML.addEventListener("click", () => exportWorkflow("yaml"));
	document.getElementById("wf-step-tool").addEventListener("change", syncStepFields);
	document.getElementById("wf-step-form").addEventListener("submit", saveStep);
	document.getElementById("wf-step-delete").addEventListener("click", deleteStepFromModal);
	document.getElementById("wf-step-draft-go").addEventListener("click", draftStep);
	document.getElementById("wf-step-draft").addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			draftStep();
		}
	});
	document.getElementById("wf-name").addEventListener("input", wfSave);
	document.getElementById("wf-inventory").addEventListener("input", wfSave);
	const modal = document.getElementById("wf-step-modal");
	const card = modal.querySelector(".modal-card");
	card.setAttribute("role", "dialog");
	card.setAttribute("aria-modal", "true");
	document.getElementById("wf-step-close").addEventListener("click", closeStepModal);
	modal.addEventListener("click", (e) => { if (e.target === modal) closeStepModal(); });
	modal.addEventListener("keydown", (e) => {
		if (e.key !== "Tab" || modal.hidden) return;
		const focusable = modal.querySelectorAll(
			"button:not([disabled]):not([hidden]), input:not([disabled]), " +
			"select:not([disabled]), textarea:not([disabled])");
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	});
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !modal.hidden) { closeStepModal(); return; }
		if (e.key === "Escape" && wfState.linkFrom) {
			wfState.linkFrom = null;
			wfSetStatus("", "");
			return;
		}
		const tag = e.target.tagName;
		if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || !modal.hidden) return;
		const mod = e.metaKey || e.ctrlKey;
		if (mod && !e.shiftKey && e.key.toLowerCase() === "z") {
			e.preventDefault();
			wfUndo();
		} else if ((mod && e.shiftKey && e.key.toLowerCase() === "z") || (mod && e.key.toLowerCase() === "y")) {
			e.preventDefault();
			wfRedo();
		} else if ((e.key === "Delete" || e.key === "Backspace") && wfState.selectedEdge) {
			e.preventDefault();
			removeEdge(wfState.selectedEdge);
		} else if (e.key === "+" || e.key === "=") {
			e.preventDefault();
			zoomBy(1.2);
		} else if (e.key === "-" || e.key === "_") {
			e.preventDefault();
			zoomBy(1 / 1.2);
		} else if (e.key === "0") {
			e.preventDefault();
			resetView();
		} else if (e.key.toLowerCase() === "f") {
			e.preventDefault();
			fitView();
		}
	});
	canvas.addEventListener("pointerdown", wfPointerDown);
	canvas.addEventListener("pointermove", wfPointerMove);
	canvas.addEventListener("pointerup", wfPointerUp);
	canvas.addEventListener("pointercancel", wfCancelPointer);
	canvas.addEventListener("lostpointercapture", wfCancelPointer);
	canvas.addEventListener("wheel", wfWheel, { passive: false });
	document.getElementById("wf-zoom-in").addEventListener("click", () => zoomBy(1.2));
	document.getElementById("wf-zoom-out").addEventListener("click", () => zoomBy(1 / 1.2));
	document.getElementById("wf-zoom-fit").addEventListener("click", fitView);
	document.getElementById("wf-zoom-level").addEventListener("click", fitView);
	wfState.edgesLayer.addEventListener("click", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit) selectEdge(hit.dataset.from, hit.dataset.to);
	});
	wfState.edgesLayer.addEventListener("focusin", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit) selectEdge(hit.dataset.from, hit.dataset.to);
	});
	wfState.edgesLayer.addEventListener("keydown", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit && (e.key === "Delete" || e.key === "Backspace" || e.key === "Enter")) {
			e.preventDefault();
			removeEdge({ from: hit.dataset.from, to: hit.dataset.to });
		}
	});
	window.addEventListener("resize", renderEdges);
	mountWizard();
	let hadViewport = wfRestore();
	if (!wfState.nodes.length) {
		wfSeedExample();
		hadViewport = false;
	}
	renderWorkflow();
	if (hadViewport) {
		applyViewport();
	} else if (wfState.nodes.length > 0) {
		fitView();
	} else {
		applyViewport();
	}
}

// WF_MIN_SCALE and WF_MAX_SCALE bound the zoom so the graph never shrinks past legibility or
// grows past usefulness. WF_NODE_H is the node card height used for the fit bounding box.
const WF_MIN_SCALE = 0.2;
const WF_MAX_SCALE = 2.5;
const WF_NODE_H = 62;

// clampScale keeps a scale inside the allowed zoom range, and falls back to full size for a value
// that is not a finite number so a corrupt draft cannot write a broken transform.
function clampScale(k) {
	if (!Number.isFinite(k)) return 1;
	return Math.max(WF_MIN_SCALE, Math.min(WF_MAX_SCALE, k));
}

// applyViewport writes the current pan and scale to the world transform and the backing grid, and
// updates the zoom readout. It touches only styles, so pan and zoom stay on the compositor and
// never re-lay-out the graph.
function applyViewport() {
	const { world, canvas, panX, panY, scale } = wfState;
	world.style.transform = "translate(" + panX + "px, " + panY + "px) scale(" + scale + ")";
	canvas.style.backgroundSize = 22 * scale + "px " + 22 * scale + "px";
	canvas.style.backgroundPosition = panX + "px " + panY + "px";
	const label = document.getElementById("wf-zoom-level");
	if (label) label.textContent = Math.round(scale * 100) + "%";
}

// zoomAt scales toward a target scale while keeping the world point under the given screen point
// fixed, so zooming homes in on whatever the cursor is over.
function zoomAt(clientX, clientY, target) {
	const k = clampScale(target);
	const rect = wfState.canvas.getBoundingClientRect();
	const sx = clientX - rect.left;
	const sy = clientY - rect.top;
	const wx = (sx - wfState.panX) / wfState.scale;
	const wy = (sy - wfState.panY) / wfState.scale;
	wfState.scale = k;
	wfState.panX = sx - wx * k;
	wfState.panY = sy - wy * k;
	applyViewport();
	saveViewSoon();
}

// zoomBy zooms by a factor around the center of the canvas, used by the buttons and keys.
function zoomBy(factor) {
	const rect = wfState.canvas.getBoundingClientRect();
	zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, wfState.scale * factor);
}

// resetView returns to full size with the graph's top-left tucked into the corner.
function resetView() {
	wfState.scale = 1;
	wfState.panX = 40;
	wfState.panY = 40;
	applyViewport();
	wfSave();
}

// fitView frames the whole graph in the canvas with a margin, scaling down for a large graph and
// never past full size, and centers it. With no nodes it resets.
function fitView() {
	if (wfState.nodes.length === 0) {
		resetView();
		return;
	}
	let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
	for (const n of wfState.nodes) {
		minX = Math.min(minX, n.x);
		minY = Math.min(minY, n.y);
		maxX = Math.max(maxX, n.x + WF_CARD_W);
		maxY = Math.max(maxY, n.y + WF_NODE_H);
	}
	const rect = wfState.canvas.getBoundingClientRect();
	const pad = 48;
	const w = maxX - minX;
	const h = maxY - minY;
	const k = clampScale(Math.min(
		(rect.width - pad * 2) / Math.max(w, 1),
		(rect.height - pad * 2) / Math.max(h, 1),
		1));
	wfState.scale = k;
	wfState.panX = (rect.width - w * k) / 2 - minX * k;
	wfState.panY = (rect.height - h * k) / 2 - minY * k;
	applyViewport();
	wfSave();
}

// wfSave persists the graph and the toolbar fields as the working draft. Storage failures are
// ignored, since the editor still works without drafts.
function wfSave() {
	try {
		localStorage.setItem(wfDraftKey, JSON.stringify({
			nodes: wfState.nodes, edges: wfState.edges, seq: wfState.seq,
			name: document.getElementById("wf-name").value,
			inventory: document.getElementById("wf-inventory").value,
			view: { scale: wfState.scale, panX: wfState.panX, panY: wfState.panY },
		}));
	} catch { /* storage full or blocked */ }
}

// wfViewSaveTimer debounces persisting the viewport, so a burst of wheel or pan events writes the
// draft once when it settles rather than on every frame.
let wfViewSaveTimer = null;

// saveViewSoon schedules a draft save after the pan or zoom settles.
function saveViewSoon() {
	clearTimeout(wfViewSaveTimer);
	wfViewSaveTimer = setTimeout(wfSave, 250);
}

// wfSeedExample lays out a small but real pipeline: infrastructure fans out to configuration and
// database migration, both of which a smoke test waits on. It gives a first visit something to
// drag, zoom, and read instead of an empty canvas, and it is replaced the moment the reader
// changes anything, since the draft then saves over it.
function wfSeedExample() {
	const step = (id, name, tool, x, y, extra) => Object.assign({
		id, name, tool, x, y,
		playbook: tool === "ansible" ? "site.yml" : "",
		command: tool === "ansible" ? "" : "echo running " + name,
		inventory: "", dryRun: false, continueOnFailure: false, retries: 0,
	}, extra || {});
	wfState.nodes = [
		step(1, "provision", "terraform", 60, 150, { command: "infra/network", dryRun: true }),
		step(2, "configure", "ansible", 330, 60),
		step(3, "migrate-db", "ansible", 330, 250, { playbook: "migrate.yml" }),
		step(4, "smoke-test", "bash", 600, 150, { command: "curl -fsS https://example.internal/healthz", retries: 2 }),
	];
	wfState.edges = [
		{ from: 1, to: 2 }, { from: 1, to: 3 },
		{ from: 2, to: 4 }, { from: 3, to: 4 },
	];
	wfState.seq = 4;
	const name = document.getElementById("wf-name");
	if (name && !name.value) name.value = "Release pipeline";
}

// WF_PATTERNS are the shapes a real pipeline usually takes. A blank canvas asks the reader to know
// both what they want and how this editor expresses it; a pattern answers the second half, laying out
// named steps and their dependencies for them to rename and point at their own playbooks. Each step
// is (name, tool or null to take the chosen one, column, row), where null means the pattern has no
// opinion. Columns and rows are grid slots, turned into coordinates by wfApplyPattern.
const WF_PATTERNS = [
	{
		id: "linear",
		title: "One after another",
		summary: "Each step waits for the one before it.",
		detail: "The safe default. Nothing overlaps, so a failure stops the rest.",
		diagram: [[1], [1], [1]],
		steps: [
			["build", null, 0, 0],
			["test", null, 1, 0],
			["deploy", null, 2, 0],
		],
		links: [[0, 1], [1, 2]],
	},
	{
		id: "fanout",
		title: "Fan out, then gate",
		summary: "One step opens, several run at once, a last step waits for all of them.",
		detail: "Use it when independent work can overlap but nothing after it should start early.",
		diagram: [[1], [1, 1, 1], [1]],
		steps: [
			["prepare", null, 0, 1],
			["web", null, 1, 0],
			["workers", null, 1, 1],
			["database", null, 1, 2],
			["verify", null, 2, 1],
		],
		links: [[0, 1], [0, 2], [0, 3], [1, 4], [2, 4], [3, 4]],
	},
	{
		id: "provision",
		title: "Provision, then configure",
		summary: "Terraform builds the infrastructure, Ansible configures it, a check proves it.",
		detail: "The two-tool pipeline AWX cannot express without a second system.",
		diagram: [[1], [1, 1], [1]],
		steps: [
			["provision", "terraform", 0, 1],
			["configure", "ansible", 1, 0],
			["migrate-db", "ansible", 1, 1],
			["smoke-test", "bash", 2, 1],
		],
		links: [[0, 1], [0, 2], [1, 3], [2, 3]],
	},
	{
		id: "canary",
		title: "Canary, then the fleet",
		summary: "Ship to one host, verify it, then roll to the rest.",
		detail: "Stops a bad change after one host instead of across the fleet.",
		diagram: [[1], [1], [1], [1]],
		steps: [
			["deploy-canary", null, 0, 0],
			["verify-canary", "bash", 1, 0],
			["deploy-fleet", null, 2, 0],
			["verify-fleet", "bash", 3, 0],
		],
		links: [[0, 1], [1, 2], [2, 3]],
	},
];

// WF_PATTERN_COL and WF_PATTERN_ROW are the grid pitch a pattern lays out on, wide enough that cards
// never touch and their links read as curves rather than as creases.
const WF_PATTERN_COL = 270;
const WF_PATTERN_ROW = 130;

// patternByID returns the pattern with the given id, or null.
function patternByID(id) {
	return WF_PATTERNS.find((p) => p.id === id) || null;
}

// wfApplyPattern replaces the graph with the named pattern, in the chosen tool where the pattern has
// no opinion of its own. It goes through the undo stack, so a pattern dropped onto work in progress is
// one Cmd-Z away from being taken back.
function wfApplyPattern(id, tool) {
	const pattern = patternByID(id);
	if (!pattern) return;
	wfSnapshot();
	const target = (t) => (t === "ansible" ? "site.yml" : "");
	wfState.nodes = pattern.steps.map(([name, stepTool, col, row], i) => {
		const chosen = stepTool || tool;
		return {
			id: "n" + (wfState.seq + i),
			name,
			tool: chosen,
			x: 60 + col * WF_PATTERN_COL,
			y: 60 + row * WF_PATTERN_ROW,
			playbook: chosen === "ansible" ? target(chosen) : "",
			command: chosen === "ansible" ? "" : "echo " + name,
			inventory: "",
			dryRun: false,
			continueOnFailure: false,
			retries: 0,
		};
	});
	wfState.edges = pattern.links.map(([from, to]) => ({
		from: wfState.nodes[from].id, to: wfState.nodes[to].id,
	}));
	wfState.seq += pattern.steps.length;
	wfState.selectedEdge = null;
	renderWorkflow();
	fitView();
	wfSave();
	wfSetStatus("Laid out " + pattern.steps.length + " steps. Open each one to point it at your own " +
		"playbook or script. Undo takes it back.", "");
}

// patternDiagram draws a pattern's shape as a small grid of dots, so the shape is picked by looking
// rather than by reading a description of it.
function patternDiagram(pattern) {
	const wrap = document.createElement("span");
	wrap.className = "wf-pattern-shape";
	wrap.setAttribute("aria-hidden", "true");
	for (const column of pattern.diagram) {
		const col = document.createElement("span");
		col.className = "wf-pattern-col";
		for (let i = 0; i < column.length; i++) col.appendChild(document.createElement("i"));
		wrap.appendChild(col);
	}
	return wrap;
}

// mountWizard builds the pattern chooser and wires the controls that open it. Choosing a pattern
// replaces the graph, so a canvas that already holds work says so before it is overwritten.
function mountWizard() {
	const modal = document.getElementById("wf-wizard-modal");
	const list = document.getElementById("wf-wizard-list");
	if (!modal || !list) return;
	const warn = document.getElementById("wf-wizard-warn");
	const toolPick = document.getElementById("wf-wizard-tool");
	const card = modal.querySelector(".modal-card");
	card.setAttribute("role", "dialog");
	card.setAttribute("aria-modal", "true");

	const close = () => {
		modal.hidden = true;
		if (wfState.opener && wfState.opener.focus) wfState.opener.focus();
		wfState.opener = null;
	};
	const open = () => {
		wfState.opener = document.activeElement;
		if (warn) {
			warn.hidden = wfState.nodes.length === 0;
			warn.textContent = "This canvas already holds " + wfState.nodes.length +
				(wfState.nodes.length === 1 ? " step" : " steps") +
				". Choosing a pattern replaces them, and undo brings them back.";
		}
		modal.hidden = false;
		const first = list.querySelector(".wf-pattern");
		if (first) first.focus();
	};

	for (const pattern of WF_PATTERNS) {
		const item = document.createElement("button");
		item.type = "button";
		item.className = "wf-pattern";
		item.appendChild(patternDiagram(pattern));
		const text = document.createElement("span");
		text.className = "wf-pattern-text";
		const title = document.createElement("span");
		title.className = "wf-pattern-title";
		title.textContent = pattern.title;
		const summary = document.createElement("span");
		summary.className = "wf-pattern-summary";
		summary.textContent = pattern.summary;
		const detail = document.createElement("span");
		detail.className = "wf-pattern-detail";
		detail.textContent = pattern.detail;
		text.appendChild(title);
		text.appendChild(summary);
		text.appendChild(detail);
		item.appendChild(text);
		item.addEventListener("click", () => {
			wfApplyPattern(pattern.id, toolPick ? toolPick.value : "ansible");
			close();
		});
		list.appendChild(item);
	}

	document.getElementById("wf-wizard-close").addEventListener("click", close);
	modal.addEventListener("click", (e) => { if (e.target === modal) close(); });
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !modal.hidden) close();
	});
	for (const id of ["wf-wizard-open", "wf-hint-wizard"]) {
		const btn = document.getElementById(id);
		if (btn) btn.addEventListener("click", open);
	}
	const hintAdd = document.getElementById("wf-hint-add");
	if (hintAdd) hintAdd.addEventListener("click", () => openStepModal(null));
}

// wfNextSeq returns the counter the next node id is minted from: one past the highest id already in
// the graph, and never below the draft's own counter.
//
// A draft saved before the counter was stored fell back to the node count, which is not the highest
// id. Delete the second of five steps and the graph holds n0, n2, n3, n4 with a count of four, so
// the next step is minted as n4 and there are two of those: the edges of one attach to the other,
// selecting one selects both, and deleting one deletes both. Reading the ids says what the count
// only guesses at.
function wfNextSeq(nodes, seq) {
	let next = typeof seq === "number" && seq > 0 ? seq : 0;
	for (const n of nodes) {
		const found = /^n(\d+)$/.exec(String((n && n.id) || ""));
		if (found) next = Math.max(next, parseInt(found[1], 10) + 1);
	}
	return next;
}

// wfRestore loads the saved draft into the editor state, ignoring anything malformed. It reports
// whether a saved viewport was restored, so the mount can fit the graph on a first visit instead.
function wfRestore() {
	let draft = null;
	try {
		draft = JSON.parse(localStorage.getItem(wfDraftKey) || "null");
	} catch {
		return false;
	}
	if (!draft || !Array.isArray(draft.nodes) || !Array.isArray(draft.edges)) return false;
	wfState.nodes = draft.nodes;
	wfState.edges = draft.edges;
	wfState.seq = wfNextSeq(draft.nodes, draft.seq);
	document.getElementById("wf-name").value = draft.name || "";
	document.getElementById("wf-inventory").value = draft.inventory || "";
	const v = draft.view;
	if (v && Number.isFinite(v.scale) && Number.isFinite(v.panX) && Number.isFinite(v.panY)) {
		wfState.scale = clampScale(v.scale);
		wfState.panX = v.panX;
		wfState.panY = v.panY;
		return true;
	}
	return false;
}

// wfSnapshot pushes the current graph onto the undo stack before a mutation. A coalesce key merges
// rapid repeats, so a burst of arrow-key nudges undoes as one step.
function wfSnapshot(key) {
	const now = Date.now();
	if (key && wfState.lastSnapKey === key && now - wfState.lastSnapAt < 1000) {
		wfState.lastSnapAt = now;
		return;
	}
	wfState.lastSnapKey = key || null;
	wfState.lastSnapAt = now;
	wfPushHistory(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
}

// wfPushHistory records a serialized graph as an undo point and clears the redo stack.
function wfPushHistory(snapshot) {
	wfState.past.push(snapshot);
	if (wfState.past.length > wfHistoryCap) wfState.past.shift();
	wfState.future = [];
}

// wfUndo restores the previous graph state.
function wfUndo() {
	if (wfState.past.length === 0) return;
	wfState.future.push(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
	applyGraph(JSON.parse(wfState.past.pop()));
}

// wfRedo reapplies an undone graph state.
function wfRedo() {
	if (wfState.future.length === 0) return;
	wfState.past.push(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
	applyGraph(JSON.parse(wfState.future.pop()));
}

// applyGraph replaces the graph, redraws, and persists the draft, used by undo and redo.
function applyGraph(g) {
	wfState.nodes = g.nodes;
	wfState.edges = g.edges;
	wfState.selectedEdge = null;
	wfState.lastSnapKey = null;
	renderWorkflow();
	wfSave();
}

// wfPoint converts a pointer event to world coordinates, undoing the current pan and zoom so a
// node dropped under the cursor lands where the cursor is, at any zoom.
function wfPoint(e) {
	const r = wfState.canvas.getBoundingClientRect();
	return {
		x: (e.clientX - r.left - wfState.panX) / wfState.scale,
		y: (e.clientY - r.top - wfState.panY) / wfState.scale,
	};
}

// syncStepFields shows the playbook field for Ansible and the command field for the other tools.
// The AI draft row appears only for the inline script tools, where a draft can fill the command.
function syncStepFields() {
	const tool = document.getElementById("wf-step-tool").value;
	const ansible = tool === "ansible";
	document.getElementById("wf-step-playbook-field").hidden = !ansible;
	document.getElementById("wf-step-command-field").hidden = ansible;
	const drafts = tool === "bash" || tool === "python" || tool === "go";
	document.getElementById("wf-step-draft-field").hidden = !drafts;
}

// draftStep asks the AI endpoint for a script matching the description and fills the command field
// with the draft for the user to review and edit. Advisory only: nothing runs until the step is
// saved and the workflow is submitted, through the same gates as any other run.
async function draftStep() {
	const status = document.getElementById("wf-step-status");
	const prompt = document.getElementById("wf-step-draft").value.trim();
	const tool = document.getElementById("wf-step-tool").value;
	if (!prompt) {
		status.textContent = "Describe the step first.";
		return;
	}
	const btn = document.getElementById("wf-step-draft-go");
	btn.disabled = true;
	status.textContent = "Drafting.";
	try {
		const res = await fetch(API + "/ai/draft", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ tool, prompt }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		if (res.status === 404) {
			status.textContent = "AI is not enabled on this server.";
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		document.getElementById("wf-step-command").value = data.draft || "";
		status.textContent = "Draft ready. Review it before saving.";
	} catch (err) {
		status.textContent = "Draft failed: " + err.message;
	} finally {
		btn.disabled = false;
	}
}

// openStepModal opens the step editor for a new step, or for the given node to edit it in place.
function openStepModal(node) {
	wfState.opener = document.activeElement;
	wfState.editing = node ? node.id : null;
	document.getElementById("wf-step-status").textContent = "";
	document.getElementById("wf-step-name").value = node ? node.name : "";
	document.getElementById("wf-step-tool").value = node ? node.tool : "ansible";
	document.getElementById("wf-step-playbook").value = node ? node.playbook : "";
	document.getElementById("wf-step-command").value = node ? node.command : "";
	document.getElementById("wf-step-inventory").value = node ? node.inventory : "";
	document.getElementById("wf-step-dryrun").checked = node ? node.dryRun : false;
	document.getElementById("wf-step-continue").checked = node ? node.continueOnFailure : false;
	document.getElementById("wf-step-retries").value = node ? node.retries : 0;
	document.getElementById("wf-step-delete").hidden = !node;
	syncStepFields();
	document.getElementById("wf-step-modal").hidden = false;
	document.getElementById("wf-step-name").focus();
}

// closeStepModal hides the step editor and returns focus to whatever opened it.
function closeStepModal() {
	document.getElementById("wf-step-modal").hidden = true;
	wfState.editing = null;
	if (wfState.opener && wfState.opener.focus) wfState.opener.focus();
	wfState.opener = null;
}

// saveStep validates the step form and creates or updates the node, keeping step names unique so
// dependencies stay unambiguous and requiring the tool's input so a broken step is caught here
// instead of at run time.
function saveStep(e) {
	e.preventDefault();
	const name = document.getElementById("wf-step-name").value.trim();
	const tool = document.getElementById("wf-step-tool").value;
	const status = document.getElementById("wf-step-status");
	if (!name) { status.textContent = "Name is required."; return; }
	const clash = wfState.nodes.some((n) => n.name === name && n.id !== wfState.editing);
	if (clash) { status.textContent = "A step named " + name + " already exists."; return; }
	const fields = {
		name, tool,
		playbook: document.getElementById("wf-step-playbook").value.trim(),
		command: document.getElementById("wf-step-command").value.trim(),
		inventory: document.getElementById("wf-step-inventory").value.trim(),
		dryRun: document.getElementById("wf-step-dryrun").checked,
		continueOnFailure: document.getElementById("wf-step-continue").checked,
		retries: Math.max(0, parseInt(document.getElementById("wf-step-retries").value, 10) || 0),
	};
	if (tool === "ansible" && !fields.playbook) {
		status.textContent = "An Ansible step needs a playbook.";
		return;
	}
	if (tool !== "ansible" && !fields.command) {
		status.textContent = "A " + tool + " step needs a command.";
		return;
	}
	wfSnapshot();
	if (wfState.editing) {
		const node = wfState.nodes.find((n) => n.id === wfState.editing);
		Object.assign(node, fields);
	} else {
		wfState.nodes.push(Object.assign({ id: "n" + (wfState.seq++) }, spawnPosition(), fields));
	}
	closeStepModal();
	renderWorkflow();
	wfSave();
}

// spawnPosition picks a free grid slot for a new node inside the visible part of the canvas at the
// current pan and zoom, skipping occupied spots so new steps never stack on existing ones.
function spawnPosition() {
	const rect = wfState.canvas.getBoundingClientRect();
	const viewW = rect.width / wfState.scale;
	const baseX = -wfState.panX / wfState.scale + 40;
	const baseY = -wfState.panY / wfState.scale + 40;
	const cols = Math.max(1, Math.floor((viewW - 40) / 210));
	for (let i = 0; i < 1000; i++) {
		const x = baseX + (i % cols) * 210;
		const y = baseY + Math.floor(i / cols) * 130;
		if (!wfState.nodes.some((n) => Math.abs(n.x - x) < 30 && Math.abs(n.y - y) < 30)) {
			return { x, y };
		}
	}
	return { x: baseX, y: baseY };
}

// deleteStepFromModal removes the node currently open in the editor along with its edges.
function deleteStepFromModal() {
	if (wfState.editing) removeNode(wfState.editing);
	closeStepModal();
}

// removeNode deletes a node and every edge touching it. Undo can bring it back.
function removeNode(id) {
	wfSnapshot();
	wfState.nodes = wfState.nodes.filter((n) => n.id !== id);
	wfState.edges = wfState.edges.filter((e) => e.from !== id && e.to !== id);
	if (wfState.linkFrom === id) wfState.linkFrom = null;
	renderWorkflow();
	wfSave();
}

// renderWorkflow redraws the nodes and the edges, refreshes the color key, and toggles the empty
// state.
function renderWorkflow() {
	renderNodes();
	renderEdges();
	renderLegend();
	if (wfState.hint) wfState.hint.hidden = wfState.nodes.length > 0;
}

// renderNodes reconciles the node cards with the model, positioning each and wiring its handles.
// Cards are focusable: Enter edits, arrows move, L starts a link, and Delete removes. Every card
// carries its tool on a data attribute, which is what tints the card, its badge, and its handles, so
// a graph reads as a set of distinct steps under the flat themes as much as the signature one.
function renderNodes() {
	const layer = wfState.nodesLayer;
	layer.textContent = "";
	for (const node of wfState.nodes) {
		const el = document.createElement("div");
		el.className = "wf-node";
		el.dataset.tool = node.tool;
		el.style.left = node.x + "px";
		el.style.top = node.y + "px";
		el.dataset.id = node.id;
		el.tabIndex = 0;
		el.setAttribute("role", "group");
		el.setAttribute("aria-label", node.name + ", " + node.tool +
			" step. Enter edits, arrow keys move, L starts a link, Delete removes.");
		const target = node.tool === "ansible" ? node.playbook : node.command;
		el.innerHTML =
			'<div class="wf-node-head"><span class="wf-node-name"></span>' +
			'<button type="button" class="wf-node-del" aria-label="Delete step">&times;</button></div>' +
			'<div class="wf-node-meta"><span class="wf-tool"></span><span class="wf-node-target mono"></span></div>' +
			'<div class="wf-node-flags"></div>' +
			'<span class="wf-handle wf-in" aria-hidden="true"></span>' +
			'<span class="wf-handle wf-out" data-tip="Drag onto another step to make it wait for this one"></span>';
		el.querySelector(".wf-node-name").textContent = node.name;
		el.querySelector(".wf-tool").textContent = node.tool;
		el.querySelector(".wf-node-target").textContent = target || "";
		// The settings that change how a step behaves are marked on the card, so a dry run or a step
		// that swallows its own failure is visible without opening it.
		const flags = el.querySelector(".wf-node-flags");
		const flag = (text, cls, tip) => {
			const span = document.createElement("span");
			span.className = "wf-flag " + cls;
			span.textContent = text;
			span.dataset.tip = tip;
			flags.appendChild(span);
		};
		if (node.dryRun) flag("dry", "dry", "Reports what would change without changing anything");
		if (node.continueOnFailure) {
			flag("continues", "warn", "A failure here does not stop the steps after it");
		}
		if (node.retries > 0) {
			flag("retry " + node.retries, "retry",
				"Retried up to " + node.retries + " more " + (node.retries === 1 ? "time" : "times") +
				" before it counts as failed");
		}
		flags.hidden = !flags.children.length;
		el.querySelector(".wf-node-del").addEventListener("click", (ev) => { ev.stopPropagation(); removeNode(node.id); });
		el.querySelector(".wf-out").addEventListener("pointerdown", (ev) => startLink(ev, node.id));
		el.addEventListener("pointerdown", (ev) => startDrag(ev, node.id));
		el.addEventListener("keydown", (ev) => nodeKey(ev, node.id));
		layer.appendChild(el);
	}
}

// renderLegend names the tool colors the current graph actually uses, so the hues on the canvas are
// decoded without a trip to the docs. A single-tool graph needs no key, so it does not get one.
function renderLegend() {
	const legend = document.getElementById("wf-legend");
	if (!legend) return;
	const tools = [];
	for (const node of wfState.nodes) {
		if (!tools.includes(node.tool)) tools.push(node.tool);
	}
	legend.textContent = "";
	legend.hidden = tools.length < 2;
	if (legend.hidden) return;
	for (const tool of tools.sort()) {
		const item = document.createElement("span");
		item.className = "wf-legend-item";
		item.dataset.tool = tool;
		const dot = document.createElement("i");
		item.appendChild(dot);
		item.appendChild(document.createTextNode(tool));
		legend.appendChild(item);
	}
}

// nodeKey handles keyboard interaction on a focused node: edit, move, link, and delete. Completing
// a pending link happens on Enter over the target node.
function nodeKey(e, id) {
	const node = wfState.nodes.find((n) => n.id === id);
	if (!node || e.target.closest(".wf-node-del")) return;
	if (e.key === "Enter" || e.key === " ") {
		e.preventDefault();
		if (wfState.linkFrom && wfState.linkFrom !== id) {
			linkTo(wfState.linkFrom, id);
			wfState.linkFrom = null;
			renderEdges();
		} else {
			openStepModal(node);
		}
	} else if (e.key === "Delete" || e.key === "Backspace") {
		e.preventDefault();
		removeNode(id);
	} else if (e.key.toLowerCase() === "l") {
		e.preventDefault();
		wfState.linkFrom = id;
		wfSetStatus("Linking from " + node.name +
			". Focus another step and press Enter to add the dependency. Escape cancels.", "");
	} else if (e.key.startsWith("Arrow")) {
		e.preventDefault();
		wfSnapshot("move-" + id);
		const d = e.shiftKey ? 1 : 10;
		if (e.key === "ArrowLeft") node.x = Math.max(0, node.x - d);
		if (e.key === "ArrowRight") node.x += d;
		if (e.key === "ArrowUp") node.y = Math.max(0, node.y - d);
		if (e.key === "ArrowDown") node.y += d;
		positionNode(id);
		renderEdges();
		wfSave();
	}
}

// positionNode moves a node's card in place without rebuilding the layer, so focus and listeners
// survive a drag or a keyboard move.
function positionNode(id) {
	const node = wfState.nodes.find((n) => n.id === id);
	const el = wfState.nodesLayer.querySelector('[data-id="' + id + '"]');
	if (node && el) {
		el.style.left = node.x + "px";
		el.style.top = node.y + "px";
	}
}

// renderEdges draws every dependency edge with an invisible wide hit path over it for selection,
// plus the in-progress link while one is being dragged.
function renderEdges() {
	const svg = wfState.edgesLayer;
	// The SVG lives in world space inside the transformed layer, so it is sized to the content
	// extent. Anything drawn past it still shows because the layer allows overflow.
	let extentX = 1, extentY = 1;
	for (const n of wfState.nodes) {
		extentX = Math.max(extentX, n.x + WF_CARD_W + 80);
		extentY = Math.max(extentY, n.y + WF_NODE_H + 80);
	}
	svg.setAttribute("width", extentX);
	svg.setAttribute("height", extentY);
	let paths = "";
	for (const e of wfState.edges) {
		const a = wfState.nodes.find((n) => n.id === e.from);
		const b = wfState.nodes.find((n) => n.id === e.to);
		if (!(a && b)) continue;
		const sel = wfState.selectedEdge &&
			wfState.selectedEdge.from === e.from && wfState.selectedEdge.to === e.to;
		const d = edgeD(a.x + WF_CARD_W, a.y + WF_HANDLE_Y, b.x, b.y + WF_HANDLE_Y);
		// A link takes the color of the step it leaves, so a fan-out is traceable back to its source
		// at a glance instead of resolving into one flat tangle.
		paths += '<path class="wf-edge' + (sel ? " wf-edge-selected" : "") + '" data-tool="' +
			esc(a.tool) + '" d="' + d + '"/>';
		paths += '<path class="wf-edge-hit" d="' + d + '" tabindex="0" role="button" ' +
			'data-from="' + esc(e.from) + '" data-to="' + esc(e.to) + '" ' +
			'aria-label="Dependency link, ' + esc(a.name) + ' into ' + esc(b.name) +
			'. Press Delete to remove it."/>';
	}
	if (wfState.link && wfState.link.cursor) {
		const a = wfState.nodes.find((n) => n.id === wfState.link.from);
		if (a) {
			paths += '<path class="wf-edge wf-edge-live" data-tool="' + esc(a.tool) + '" d="' +
				edgeD(a.x + WF_CARD_W, a.y + WF_HANDLE_Y, wfState.link.cursor.x, wfState.link.cursor.y) + '"/>';
		}
	}
	svg.innerHTML = paths;
}

// esc escapes a value for interpolation into an attribute of markup built as a string, so a step name
// carrying a quote or an angle bracket cannot break out of the attribute it sits in.
function esc(value) {
	return String(value == null ? "" : value)
		.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;")
		.replaceAll('"', "&quot;").replaceAll("'", "&#39;");
}

// edgeD returns the SVG cubic path data between two points, curving horizontally so edges read as
// flow.
function edgeD(x1, y1, x2, y2) {
	const dx = Math.max(40, Math.abs(x2 - x1) / 2);
	return "M" + x1 + " " + y1 + " C" + (x1 + dx) + " " + y1 + " " +
		(x2 - dx) + " " + y2 + " " + x2 + " " + y2;
}

// selectEdge marks a dependency edge as selected so Delete can remove it. The class flips in place
// rather than re-rendering, so keyboard focus on the hit path survives.
function selectEdge(from, to) {
	wfState.selectedEdge = { from, to };
	for (const p of wfState.edgesLayer.querySelectorAll(".wf-edge-selected")) {
		p.classList.remove("wf-edge-selected");
	}
	const hit = wfState.edgesLayer.querySelector(
		'.wf-edge-hit[data-from="' + from + '"][data-to="' + to + '"]');
	if (hit && hit.previousElementSibling) hit.previousElementSibling.classList.add("wf-edge-selected");
	wfSetStatus("Dependency selected. Press Delete to remove it.", "");
}

// deselectEdge clears the edge selection and its status line.
function deselectEdge() {
	if (!wfState.selectedEdge) return;
	wfState.selectedEdge = null;
	for (const p of wfState.edgesLayer.querySelectorAll(".wf-edge-selected")) {
		p.classList.remove("wf-edge-selected");
	}
	wfSetStatus("", "");
}

// removeEdge deletes a dependency edge. Undo can bring it back.
function removeEdge(sel) {
	wfSnapshot();
	wfState.edges = wfState.edges.filter((e) => !(e.from === sel.from && e.to === sel.to));
	wfState.selectedEdge = null;
	renderEdges();
	wfSetStatus("Dependency removed.", "");
	wfSave();
}

// startDrag begins moving a node with the primary button, unless the press landed on a handle or
// the delete control. The pre-drag graph is captured so a completed move becomes one undo step.
function startDrag(e, id) {
	if (e.button !== 0 || !e.isPrimary) return;
	if (e.target.closest(".wf-handle") || e.target.closest(".wf-node-del")) return;
	const node = wfState.nodes.find((n) => n.id === id);
	const p = wfPoint(e);
	wfState.drag = {
		id, dx: p.x - node.x, dy: p.y - node.y, sx: p.x, sy: p.y, moved: false,
		before: JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }),
	};
	wfState.canvas.setPointerCapture(e.pointerId);
}

// startLink begins drawing a dependency edge out of a node's output handle.
function startLink(e, id) {
	if (e.button !== 0 || !e.isPrimary) return;
	e.stopPropagation();
	wfState.link = { from: id, cursor: wfPoint(e) };
	wfState.canvas.setPointerCapture(e.pointerId);
}

// wfPointerDown starts a canvas pan on empty space or a two-finger pinch, and clears any edge
// selection. A press on a node or a handle is left to that element's own drag and link handlers.
function wfPointerDown(e) {
	wfState.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
	if (wfState.pointers.size === 2 && !wfState.drag && !wfState.link) {
		startPinch();
		wfState.pan = null;
		return;
	}
	const onControl = e.target.closest(".wf-node") || e.target.closest(".wf-edge-hit") ||
		e.target.closest(".wf-zoom");
	if (onControl || wfState.drag || wfState.link || e.button !== 0) return;
	deselectEdge();
	wfState.pan = { x: e.clientX, y: e.clientY, px: wfState.panX, py: wfState.panY };
	wfState.canvas.classList.add("wf-panning");
	wfState.canvas.setPointerCapture(e.pointerId);
}

