// Nav condenses on scroll and toggles a mobile menu.
const nav = document.getElementById("nav");
const onScroll = () => nav.classList.toggle("scrolled", window.scrollY > 12);
onScroll();
window.addEventListener("scroll", onScroll, { passive: true });

const toggle = document.getElementById("nav-toggle");
if (toggle) toggle.addEventListener("click", () => nav.classList.toggle("open"));
for (const a of document.querySelectorAll(".nav-links a")) {
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
