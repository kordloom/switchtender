// CARD_QUERY is the width below which every list table shows its rows as stacked cards. Above it a
// table still turns to cards when its columns outgrow their wrapper, so no list scrolls sideways.
const CARD_QUERY = "(max-width: 767px)";

// mountTableCards labels every list table's cells with their column headers and switches a table
// to stacked cards when the screen is a phone's or the table is wider than its wrapper. Rows and
// whole tables arrive after load, from fetches and from pages that build tables at runtime, so one
// observer on the content relabels and remeasures whatever changed. Where the page runs without
// the observer and media query APIs, the cells are labeled once and the tables stay tables.
function mountTableCards() {
	const main = document.querySelector("main.content");
	if (!main) return;
	if (typeof MutationObserver === "undefined" || typeof ResizeObserver === "undefined" || !window.matchMedia) {
		for (const table of main.querySelectorAll("table.runs")) labelTableCells(table);
		return;
	}
	const phone = window.matchMedia(CARD_QUERY);
	const widths = new WeakMap();
	const watched = new WeakSet();
	const sizer = new ResizeObserver((entries) => {
		for (const entry of entries) {
			const width = Math.round(entry.contentRect.width);
			if (widths.get(entry.target) === width) continue;
			widths.set(entry.target, width);
			const table = entry.target.querySelector("table.runs");
			if (table) setCardMode(table, phone.matches);
		}
	});
	const refresh = () => {
		for (const table of main.querySelectorAll("table.runs")) {
			labelTableCells(table);
			const box = cardBox(table);
			if (box && !watched.has(box)) {
				watched.add(box);
				sizer.observe(box);
			}
			setCardMode(table, phone.matches);
		}
	};
	new MutationObserver(refresh).observe(main, {
		childList: true,
		subtree: true,
		attributes: true,
		attributeFilter: ["hidden"],
	});
	phone.addEventListener("change", refresh);
	refresh();
}

// cardBox returns the element whose width a table must fit: its scroll wrapper where it has one,
// otherwise its parent.
function cardBox(table) {
	return table.closest(".list-scroll") || table.parentElement;
}

// setCardMode turns a table's cards on or off. A phone always gets cards. Wider screens get them
// only when the table's natural width, measured with cards off, exceeds its wrapper. A hidden table
// has no width to measure and is measured again when it is shown.
function setCardMode(table, phone) {
	let cards = phone;
	if (!phone) {
		const box = cardBox(table);
		if (table.hidden || !box) return;
		delete table.dataset.cards;
		cards = table.offsetWidth > box.clientWidth + 1;
	}
	if (cards) table.dataset.cards = "";
	else delete table.dataset.cards;
}

// labelTableCells copies each column's header text onto its cells as data-label and gives each
// cell its card slot as data-slot. A header names its slot with data-card where the template sets
// one. Otherwise the row number is hidden, a header reading Actions or left empty holds the row's
// controls, the first remaining column is the title, and the rest are fields. Data attributes carry
// both because row renderers overwrite className.
function labelTableCells(table) {
	const head = table.tHead && table.tHead.rows[0];
	if (!head) return;
	const cols = [];
	for (const th of head.cells) {
		const label = th.textContent.trim();
		let slot = th.dataset.card || "";
		if (!slot && th.classList.contains("col-num")) slot = "hide";
		else if (!slot && (th.classList.contains("col-actions") || label === "")) slot = "actions";
		for (let i = 0; i < (th.colSpan || 1); i++) cols.push({ label, slot });
	}
	if (!cols.some((c) => c.slot === "title")) {
		const first = cols.find((c) => !c.slot);
		if (first) first.slot = "title";
	}
	for (const body of table.tBodies) {
		for (const row of body.rows) {
			if (row.cells.length === 1 && row.cells[0].colSpan > 1) {
				setCellSlot(row.cells[0], "", "wide");
				continue;
			}
			let index = 0;
			for (const cell of row.cells) {
				const col = cols[index] || { label: "", slot: "field" };
				setCellSlot(cell, col.label, col.slot || "field");
				index += cell.colSpan || 1;
			}
		}
	}
}

// setCellSlot writes a cell's label and slot, skipping writes that would change nothing so the
// attribute observers elsewhere on the page are not woken for no reason.
function setCellSlot(cell, label, slot) {
	if (cell.dataset.label !== label) cell.dataset.label = label;
	if (cell.dataset.slot !== slot) cell.dataset.slot = slot;
}
