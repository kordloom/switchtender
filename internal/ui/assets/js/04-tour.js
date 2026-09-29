// tourStepPage returns the page a step runs on, falling back to the tour's home page for steps
// that do not hop.
function tourStepPage(tour, idx) {
	const step = tour.steps[idx];
	return (step && step.page) || tour.page;
}

// toggleTourMenu opens or closes the guided-tour launcher, a small menu of the available tours
// anchored under the Tour link in the top bar.
function toggleTourMenu(button, wrap) {
	if (wrap.querySelector(".tour-menu")) {
		closeTourMenu();
		return;
	}
	const menu = document.createElement("div");
	menu.className = "tour-menu";
	menu.setAttribute("role", "menu");
	for (const t of TOURS) {
		const item = document.createElement("button");
		item.type = "button";
		item.className = "tour-menu-item";
		item.setAttribute("role", "menuitem");
		item.innerHTML = '<span class="tour-menu-title"></span><span class="tour-menu-desc muted"></span>';
		item.querySelector(".tour-menu-title").textContent = t.title;
		item.querySelector(".tour-menu-desc").textContent = t.desc;
		item.addEventListener("click", () => launchTour(t.id));
		menu.appendChild(item);
	}
	wrap.appendChild(menu);
	button.setAttribute("aria-expanded", "true");
	const first = menu.querySelector(".tour-menu-item");
	if (first) first.focus();
	window.setTimeout(() => {
		document.addEventListener("click", tourMenuOutside);
		document.addEventListener("keydown", tourMenuKey);
	}, 0);
}

// closeTourMenu removes the launcher menu and its listeners, returning focus to the Tour button
// when the close came from the keyboard.
function closeTourMenu(restoreFocus) {
	const menu = document.querySelector(".tour-menu");
	if (menu) menu.remove();
	const btn = document.querySelector(".tour-start");
	if (btn) {
		btn.setAttribute("aria-expanded", "false");
		if (restoreFocus) btn.focus();
	}
	document.removeEventListener("click", tourMenuOutside);
	document.removeEventListener("keydown", tourMenuKey);
}

// tourMenuOutside closes the launcher when a click lands outside it.
function tourMenuOutside(e) {
	if (!e.target.closest(".tour-launch")) closeTourMenu(false);
}

// tourMenuKey drives the launcher from the keyboard: arrows rove through the tours and Escape
// closes, handing focus back to the Tour button.
function tourMenuKey(e) {
	if (e.key === "Escape") {
		closeTourMenu(true);
		return;
	}
	if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
	const items = Array.from(document.querySelectorAll(".tour-menu-item"));
	if (items.length === 0) return;
	e.preventDefault();
	const idx = items.indexOf(document.activeElement);
	const next = e.key === "ArrowDown"
		? items[(idx + 1 + items.length) % items.length]
		: items[(idx - 1 + items.length) % items.length];
	next.focus();
}

// launchTour runs a tour by id, navigating to its page first when the tour lives elsewhere and
// resuming it there through a timestamped session handoff.
function launchTour(id) {
	closeTourMenu(false);
	const tour = tourByID(id);
	if (!tour) return;
	if (tour.page === document.body.dataset.page) {
		startTour(id, { auto: !!tour.auto });
	} else {
		sessionStorage.setItem("st_tour_start", JSON.stringify({ id, auto: !!tour.auto, at: Date.now() }));
		window.location.assign(tour.path);
	}
}

// tourState holds the running tour's overlay elements and current step, or null when no tour is open.
let tourState = null;

// mountTour starts a tour requested from the launcher on another page, and shows the welcome tour
// once on a first visit to the overview. The welcome timer yields to a tour that is already
// running and to a sign-in redirect in flight.
function mountTour() {
	const pending = readPendingTour();
	if (pending) {
		const tour = tourByID(pending.id);
		if (tour && tourStepPage(tour, pending.step) === document.body.dataset.page) {
			window.setTimeout(() => startTour(pending.id, pending), 300);
			return;
		}
	}
	if (document.body.dataset.page !== "overview") return;
	if (localStorage.getItem("st_tour_done")) return;
	window.setTimeout(() => {
		if (!tourState && !window.ymRedirecting) startTour("welcome");
	}, 400);
}

// readPendingTour consumes the cross-page tour handoff, ignoring an entry older than a minute so a
// failed navigation cannot surprise-start a tour on a later organic visit.
function readPendingTour() {
	const raw = sessionStorage.getItem("st_tour_start");
	if (!raw) return null;
	sessionStorage.removeItem("st_tour_start");
	try {
		const p = JSON.parse(raw);
		if (p && p.id && Date.now() - p.at < 60000) {
			return { id: p.id, step: p.step || 0, auto: !!p.auto };
		}
	} catch { /* stale or malformed handoff */ }
	return null;
}

// startTour builds the spotlight overlay for the named tour and shows a step, the first by default
// or a later one when resuming after a page hop. Calling it while a tour runs restarts it. When
// opts.auto is set the tour drives itself, advancing on a per-step timer until paused.
function startTour(tourId, opts) {
	const tour = tourByID(tourId) || TOURS[0];
	endTour(false);
	const blocker = document.createElement("div");
	blocker.className = "tour-blocker";
	const hole = document.createElement("div");
	hole.className = "tour-hole";
	const pop = document.createElement("div");
	pop.className = "tour-pop";
	pop.setAttribute("role", "dialog");
	pop.setAttribute("aria-modal", "true");
	pop.setAttribute("aria-labelledby", "tour-title");
	pop.innerHTML =
		'<div class="tour-pop-body"><h3 class="tour-title" id="tour-title"></h3>' +
		'<p class="tour-text"></p></div>' +
		'<div class="tour-foot"><span class="tour-count muted"></span>' +
		'<div class="tour-btns">' +
		'<button type="button" class="button tour-play" aria-pressed="false" hidden>Pause</button>' +
		'<button type="button" class="button tour-skip">Skip</button>' +
		'<button type="button" class="button tour-back">Back</button>' +
		'<button type="button" class="button primary tour-next">Next</button>' +
		"</div></div>" +
		'<div class="tour-bar" aria-hidden="true"></div>';
	document.body.appendChild(blocker);
	document.body.appendChild(hole);
	document.body.appendChild(pop);

	tourState = {
		tour, step: (opts && opts.step) || 0, steps: tour.steps, blocker, hole, pop,
		auto: !!(opts && opts.auto), timer: 0,
	};
	pop.querySelector(".tour-play").addEventListener("click", () => setTourAuto(!tourState.auto));
	pop.querySelector(".tour-skip").addEventListener("click", () => endTour(true));
	pop.querySelector(".tour-back").addEventListener("click", () => moveTour(-1));
	pop.querySelector(".tour-next").addEventListener("click", () => moveTour(1));
	pop.addEventListener("keydown", (e) => {
		if (e.key !== "Tab") return;
		const focusable = pop.querySelectorAll("button:not([hidden])");
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
	document.addEventListener("keydown", tourKey);
	window.addEventListener("resize", tourReflow);
	window.addEventListener("scroll", tourReflow, true);
	showTourStep();
}

// moveTour advances or rewinds the tour, ending it past the last step. A step that lives on
// another page hands the tour off through sessionStorage and navigates there. Manual movement
// pauses a self-driving tour; movement from the step timer keeps it rolling.
function moveTour(delta, fromAuto) {
	if (!tourState) return;
	clearTourTimer();
	if (!fromAuto) tourState.auto = false;
	const next = tourState.step + delta;
	if (next >= tourState.steps.length) {
		endTour(true);
		return;
	}
	if (next < 0) return;
	const page = tourStepPage(tourState.tour, next);
	if (page !== document.body.dataset.page) {
		const step = tourState.steps[next];
		sessionStorage.setItem("st_tour_start", JSON.stringify({
			id: tourState.tour.id, step: next, auto: tourState.auto, at: Date.now(),
		}));
		window.location.assign(step.path || tourState.tour.path);
		return;
	}
	tourState.step = next;
	showTourStep();
}

// setTourAuto starts or stops the tour's self-advance and re-renders the step so the timer,
// progress bar, and Play control match.
function setTourAuto(on) {
	if (!tourState) return;
	tourState.auto = on;
	showTourStep();
}

// clearTourTimer cancels a pending self-advance.
function clearTourTimer() {
	if (!tourState || !tourState.timer) return;
	window.clearTimeout(tourState.timer);
	tourState.timer = 0;
}

// showTourStep fills the popover for the current step, scrolls its target into view, positions the
// spotlight, and focuses the control that drives the tour: Play while self-advancing, Next
// otherwise. On a self-driving tour it also arms the step timer and runs the progress bar.
function showTourStep() {
	if (!tourState) return;
	clearTourTimer();
	const step = tourState.steps[tourState.step];
	const { pop } = tourState;
	pop.querySelector(".tour-title").textContent = step.title;
	pop.querySelector(".tour-text").textContent = step.body;
	pop.querySelector(".tour-count").textContent = (tourState.step + 1) + " / " + tourState.steps.length;
	pop.querySelector(".tour-back").hidden = tourState.step === 0;
	const isLast = tourState.step === tourState.steps.length - 1;
	pop.querySelector(".tour-next").textContent = isLast ? "Explore" : "Next";
	pop.querySelector(".tour-skip").hidden = isLast;

	const play = pop.querySelector(".tour-play");
	play.hidden = !tourState.tour.auto;
	play.textContent = tourState.auto ? "Pause" : "Play";
	play.setAttribute("aria-pressed", tourState.auto ? "true" : "false");
	const bar = pop.querySelector(".tour-bar");
	bar.style.transition = "none";
	bar.style.width = "0";
	if (tourState.auto) {
		const hold = step.hold || 6500;
		void bar.offsetWidth;
		bar.style.transition = "width " + hold + "ms linear";
		bar.style.width = "100%";
		tourState.timer = window.setTimeout(() => moveTour(1, true), hold);
	}

	const el = tourTarget(step);
	if (el) el.scrollIntoView({ block: "center", inline: "nearest" });
	renderTourPosition();
	(tourState.auto ? play : pop.querySelector(".tour-next")).focus();
}

// renderTourPosition places the spotlight and popover for the current step without scrolling, so it
// is safe to call on scroll and resize.
function renderTourPosition() {
	if (!tourState) return;
	const step = tourState.steps[tourState.step];
	const el = tourTarget(step);
	if (el) {
		placeTourAt(el.getBoundingClientRect());
	} else {
		placeTourCentered();
	}
}

// tourTarget resolves a step's selector to the first visible match. A selector can list
// alternatives separated by a pipe, so one step can point at the docked sidebar on wide viewports
// and the drawer toggle on narrow ones.
function tourTarget(step) {
	if (!step.sel) return null;
	for (const sel of step.sel.split("|")) {
		const el = document.querySelector(sel.trim());
		if (el && el.getClientRects().length) return el;
	}
	return null;
}

// placeTourAt cuts the spotlight hole to a target rect and floats the popover below it, or above when
// there is more room there, clamped to the viewport.
function placeTourAt(rect) {
	const { hole, pop } = tourState;
	const pad = 6;
	hole.style.width = (rect.width + pad * 2) + "px";
	hole.style.height = (rect.height + pad * 2) + "px";
	hole.style.top = (rect.top - pad) + "px";
	hole.style.left = (rect.left - pad) + "px";

	const gap = 12;
	const pw = Math.min(340, window.innerWidth - 24);
	pop.style.width = pw + "px";
	const ph = pop.offsetHeight;
	let top = rect.bottom + gap;
	if (top + ph > window.innerHeight - 12 && rect.top - gap - ph > 12) {
		top = rect.top - gap - ph;
	}
	top = Math.max(12, Math.min(top, window.innerHeight - ph - 12));
	let left = rect.left + rect.width / 2 - pw / 2;
	left = Math.max(12, Math.min(left, window.innerWidth - pw - 12));
	pop.style.top = top + "px";
	pop.style.left = left + "px";
}

// placeTourCentered collapses the hole to a full dim and centers the popover for a step with no
// target.
function placeTourCentered() {
	const { hole, pop } = tourState;
	hole.style.width = "0px";
	hole.style.height = "0px";
	hole.style.top = "50%";
	hole.style.left = "50%";
	const pw = Math.min(360, window.innerWidth - 24);
	pop.style.width = pw + "px";
	pop.style.top = Math.max(12, window.innerHeight / 2 - pop.offsetHeight / 2) + "px";
	pop.style.left = (window.innerWidth / 2 - pw / 2) + "px";
}

// tourReflow repositions the current step when the window scrolls or resizes.
function tourReflow() {
	renderTourPosition();
}

// tourKey drives the tour from the keyboard: Escape ends it and arrows move between steps. Enter
// advances only when focus is not on a tour button, so a focused Back or Skip activates normally.
function tourKey(e) {
	if (!tourState) return;
	if (e.key === "Escape") {
		endTour(true);
	} else if (e.key === "Enter") {
		if (e.target.closest && e.target.closest(".tour-btns")) return;
		e.preventDefault();
		moveTour(1);
	} else if (e.key === "ArrowRight") {
		e.preventDefault();
		moveTour(1);
	} else if (e.key === "ArrowLeft") {
		e.preventDefault();
		moveTour(-1);
	}
}

// endTour tears down the overlay and its listeners, handing focus back to the Tour button so a
// keyboard user is not dropped at the top of the page. When completed is true it records that the
// tour has been seen so it does not auto-start again.
function endTour(completed) {
	if (completed) localStorage.setItem("st_tour_done", "1");
	if (!tourState) return;
	clearTourTimer();
	document.removeEventListener("keydown", tourKey);
	window.removeEventListener("resize", tourReflow);
	window.removeEventListener("scroll", tourReflow, true);
	tourState.blocker.remove();
	tourState.hole.remove();
	tourState.pop.remove();
	tourState = null;
	const btn = document.querySelector(".tour-start");
	if (btn) btn.focus();
}

// WF_CARD_W is the fixed node width used to anchor edge endpoints; WF_HANDLE_Y is the handle's
// vertical offset from a node's top, so edges leave and enter at the same height.
const WF_CARD_W = 190;
const WF_HANDLE_Y = 26;

// wfState holds the workflow graph and interaction state while the editor is open.
let wfState = null;

