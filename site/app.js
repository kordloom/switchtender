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

// Copy the install snippet.
const copy = document.getElementById("copy");
if (copy) {
	copy.addEventListener("click", async () => {
		const code = document.querySelector(".code code").innerText;
		try {
			await navigator.clipboard.writeText(code);
			copy.textContent = "Copied ✓";
			copy.classList.add("done");
			setTimeout(() => { copy.textContent = "Copy"; copy.classList.remove("done"); }, 1800);
		} catch {
			copy.textContent = "Copy failed";
		}
	});
}
