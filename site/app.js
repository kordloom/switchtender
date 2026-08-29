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
}, { threshold: 0.12, rootMargin: "0px 0px -8% 0px" });
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
