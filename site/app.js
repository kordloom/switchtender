// Nav condenses on scroll and toggles a mobile menu.
const nav = document.getElementById("nav");
const onScroll = () => nav.classList.toggle("scrolled", window.scrollY > 12);
onScroll();
window.addEventListener("scroll", onScroll, { passive: true });

const toggle = document.getElementById("nav-toggle");
if (toggle) toggle.addEventListener("click", () => nav.classList.toggle("open"));
// Any nav link or CTA button closes the open drawer, so tapping Get started from the mobile menu
// dismisses it rather than leaving it open over the destination.
for (const a of document.querySelectorAll(".nav-links a, .nav-cta a")) {
	a.addEventListener("click", () => nav.classList.remove("open"));
}

// Reveal sections as they scroll into view.
const io = new IntersectionObserver((entries) => {
	for (const e of entries) {
		if (e.isIntersecting) {
			e.target.classList.add("in");
			io.unobserve(e.target);
		}
	}
}, { threshold: 0, rootMargin: "0px 0px -8% 0px" });
for (const el of document.querySelectorAll(".reveal")) io.observe(el);

// Copy any code block: each .copy button copies the code in its own .code container.
for (const btn of document.querySelectorAll(".copy")) {
	btn.addEventListener("click", async () => {
		const block = btn.closest(".code");
		const code = block ? block.querySelector("code").innerText : "";
		try {
			await navigator.clipboard.writeText(code);
			btn.textContent = "Copied ✓";
			btn.classList.add("done");
			setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1800);
		} catch {
			btn.textContent = "Copy failed";
		}
	});
}

// UTLX easter egg: type "utlx" (or tap the footer logo five times) and a
// Union Tank Car crosses the yard.
const utlxSummon = () => {
	if (document.querySelector(".utlx")) return;
	const strip = document.createElement("div");
	strip.className = "utlx";
	strip.setAttribute("aria-hidden", "true");
	strip.innerHTML = `
	<div class="utlx-note">UTLX &middot; Union Tank Car Company &middot; <b>for mom</b></div>
	<div class="utlx-rail"></div>
	<div class="utlx-car">
		<svg viewBox="0 0 210 72" fill="none" xmlns="http://www.w3.org/2000/svg">
			<rect x="86" y="2" width="20" height="12" rx="3" fill="#14181d" stroke="rgba(230,237,243,0.28)"/>
			<rect x="10" y="10" width="172" height="36" rx="18" fill="#14181d" stroke="rgba(230,237,243,0.28)"/>
			<text x="48" y="34" font-family="JetBrains Mono, ui-monospace, monospace" font-size="15" font-weight="700" letter-spacing="3" fill="#e6edf3">UTLX</text>
			<rect x="2" y="46" width="188" height="4" rx="1" fill="#0c0f13" stroke="rgba(230,237,243,0.22)"/>
			<rect x="190" y="46" width="14" height="3" fill="#0c0f13"/>
			<g class="utlx-wheel"><circle cx="42" cy="58" r="8" fill="#0c0f13" stroke="rgba(230,237,243,0.35)"/><line x1="42" y1="51.5" x2="42" y2="64.5" stroke="rgba(230,237,243,0.35)"/></g>
			<g class="utlx-wheel"><circle cx="66" cy="58" r="8" fill="#0c0f13" stroke="rgba(230,237,243,0.35)"/><line x1="66" y1="51.5" x2="66" y2="64.5" stroke="rgba(230,237,243,0.35)"/></g>
			<g class="utlx-wheel"><circle cx="126" cy="58" r="8" fill="#0c0f13" stroke="rgba(230,237,243,0.35)"/><line x1="126" y1="51.5" x2="126" y2="64.5" stroke="rgba(230,237,243,0.35)"/></g>
			<g class="utlx-wheel"><circle cx="150" cy="58" r="8" fill="#0c0f13" stroke="rgba(230,237,243,0.35)"/><line x1="150" y1="51.5" x2="150" y2="64.5" stroke="rgba(230,237,243,0.35)"/></g>
		</svg>
	</div>`;
	document.body.appendChild(strip);
	setTimeout(() => strip.remove(), 9800);
};
let utlxKeys = "";
window.addEventListener("keydown", (e) => {
	if (e.target instanceof Element && e.target.matches("input, textarea, select, [contenteditable]")) return;
	utlxKeys = (utlxKeys + e.key.toLowerCase()).slice(-4);
	if (utlxKeys === "utlx") utlxSummon();
});
const footLogo = document.querySelector(".foot-logo");
if (footLogo) {
	let utlxTaps = 0;
	let utlxTapTimer = 0;
	footLogo.addEventListener("click", () => {
		clearTimeout(utlxTapTimer);
		utlxTaps += 1;
		if (utlxTaps >= 5) { utlxTaps = 0; utlxSummon(); return; }
		utlxTapTimer = setTimeout(() => { utlxTaps = 0; }, 1600);
	});
}

// Email capture. The address goes to the shared signup service, tagged with this site's hostname so
// a signup stays attributable across properties.
(function () {
	var form = document.getElementById("signup-form");
	if (!form) return;
	var email = document.getElementById("signup-email");
	var honeypot = document.getElementById("nf-hp");
	var button = document.getElementById("signup-submit");
	var status = document.getElementById("signup-status");

	// say writes the status line. It is a live region, so a screen reader announces the result
	// without the visitor having to go looking for it.
	function say(text, kind) {
		status.textContent = text;
		status.className = "signup-status" + (kind ? " " + kind : "");
	}

	// looksLikeEmail is a shape check, not validation. It catches an obvious typo without a round
	// trip; the server decides what is actually deliverable.
	function looksLikeEmail(value) {
		return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
	}

	form.addEventListener("submit", function (e) {
		e.preventDefault();
		var address = email.value.trim();
		if (!looksLikeEmail(address)) {
			say("That does not look like an email address.", "err");
			email.focus();
			return;
		}
		button.disabled = true;
		say("Adding you.");

		fetch("https://signup.kordloom.com/subscribe", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				email: address,
				source: window.location.hostname,
				company: honeypot.value
			})
		}).then(function (r) {
			if (!r.ok) throw new Error(String(r.status));
			// A repeat signup also succeeds, so nobody is told they are already on the list.
			form.style.display = "none";
			say("You are on the list. Thank you.", "ok");
		}).catch(function () {
			button.disabled = false;
			say("That did not go through. Email hello@kordloom.com and I will add you.", "err");
		});
	});
})();

// Decks: one card at a time from a stack, cycled with the arrows, the dots, the arrow keys, or a
// swipe. The controls are built here, so a page without script keeps a plain list of cards and no
// buttons that do nothing.
const chevron = (d) => `<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><polyline points="${d}"/></svg>`;
for (const deck of document.querySelectorAll("[data-deck]")) {
	const cards = [...deck.querySelectorAll(".deck-stack > .card")];
	if (cards.length < 2) continue;
	deck.classList.add("deck-on");
	deck.setAttribute("role", "region");
	deck.setAttribute("aria-roledescription", "carousel");
	deck.tabIndex = 0;

	const controls = document.createElement("div");
	controls.className = "deck-controls";
	const arrow = (label, points) => {
		const b = document.createElement("button");
		b.className = "deck-arrow";
		b.type = "button";
		b.setAttribute("aria-label", label);
		b.innerHTML = chevron(points);
		return b;
	};
	const prev = arrow("Previous card", "15 18 9 12 15 6");
	const next = arrow("Next card", "9 18 15 12 9 6");
	const dots = document.createElement("div");
	dots.className = "deck-dots";
	const dotFor = cards.map((card, i) => {
		const d = document.createElement("button");
		d.className = "deck-dot";
		d.type = "button";
		d.setAttribute("aria-label", card.querySelector("h3")?.textContent || `Card ${i + 1}`);
		d.addEventListener("click", () => show(i));
		dots.append(d);
		return d;
	});
	controls.append(prev, dots, next);
	deck.append(controls);

	let current = 0;
	const show = (i) => {
		current = (i + cards.length) % cards.length;
		cards.forEach((card, k) => {
			const pos = (k - current + cards.length) % cards.length;
			card.dataset.pos = pos <= 2 ? String(pos) : "hidden";
			card.inert = pos !== 0;
			card.setAttribute("aria-hidden", String(pos !== 0));
		});
		dotFor.forEach((d, k) => d.setAttribute("aria-current", String(k === current)));
	};
	prev.addEventListener("click", () => show(current - 1));
	next.addEventListener("click", () => show(current + 1));
	deck.addEventListener("keydown", (e) => {
		if (e.key === "ArrowLeft") show(current - 1);
		if (e.key === "ArrowRight") show(current + 1);
	});
	// A horizontal swipe on the stack turns the deck; a vertical one is left to scroll the page.
	let startX = null;
	let startY = null;
	const stack = deck.querySelector(".deck-stack");
	stack.addEventListener("pointerdown", (e) => { startX = e.clientX; startY = e.clientY; });
	stack.addEventListener("pointerup", (e) => {
		if (startX === null) return;
		const dx = e.clientX - startX;
		if (Math.abs(dx) > 40 && Math.abs(dx) > Math.abs(e.clientY - startY)) show(current + (dx < 0 ? 1 : -1));
		startX = null;
	});
	show(0);
}
