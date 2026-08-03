// clock.mjs is the virtual timer the sandbox runs on. Real timers in a test harness are two
// problems: a page that sets a one second interval keeps the test process alive forever, and a
// debounce nobody advances turns into a sleep in the test. Here nothing fires until a test asks for
// it, so a debounce, a coalescing window, and a polling interval are all driven deliberately.

// createClock returns a virtual clock plus the timer functions to install on a sandbox.
export function createClock() {
	// pending holds every scheduled callback, keyed by the id handed back to the caller.
	const pending = new Map();
	let nextID = 1;
	const clock = {
		// now is the current virtual time in milliseconds.
		now: 0,

		// setTimeout schedules a callback for a virtual delay and returns its cancel handle.
		setTimeout(fn, delay, ...args) {
			const id = nextID++;
			pending.set(id, { id, at: clock.now + Math.max(0, Number(delay) || 0), fn, args, every: 0 });
			return id;
		},

		// setInterval schedules a repeating callback on a virtual period.
		setInterval(fn, period, ...args) {
			const id = nextID++;
			const every = Math.max(1, Number(period) || 1);
			pending.set(id, { id, at: clock.now + every, fn, args, every });
			return id;
		},

		// clearTimeout and clearInterval both drop a scheduled callback.
		clearTimeout(id) { pending.delete(id); },
		clearInterval(id) { pending.delete(id); },

		// requestAnimationFrame schedules a callback one frame out, which is close enough for the
		// count-up animation to be driven or ignored deliberately rather than by accident.
		requestAnimationFrame(fn) {
			return clock.setTimeout(() => fn(clock.now), 16);
		},

		// cancelAnimationFrame drops a scheduled frame callback.
		cancelAnimationFrame(id) { pending.delete(id); },

		// count reports how many callbacks are still scheduled.
		count() { return pending.size; },

		// tick advances virtual time by ms, running every callback that comes due in order and
		// letting the promise queue drain between them. It does not await what a callback returns:
		// a callback whose promise only settles on a later timer would deadlock the test rather
		// than report anything, so the caller ticks again instead.
		async tick(ms) {
			const target = clock.now + Math.max(0, Number(ms) || 0);
			for (let guard = 0; guard < 100000; guard++) {
				let next = null;
				for (const t of pending.values()) {
					if (t.at <= target && (next === null || t.at < next.at || (t.at === next.at && t.id < next.id))) {
						next = t;
					}
				}
				if (!next) break;
				clock.now = next.at;
				if (next.every > 0) next.at += next.every;
				else pending.delete(next.id);
				next.fn(...next.args);
				await flush();
			}
			clock.now = target;
			await flush();
		},

		// runPending runs every scheduled one-shot callback whatever its delay, for a test that only
		// wants the queue emptied. Intervals are dropped rather than run forever.
		async runPending() {
			for (const t of [...pending.values()]) {
				if (t.every > 0) pending.delete(t.id);
			}
			let latest = clock.now;
			for (const t of pending.values()) latest = Math.max(latest, t.at);
			await clock.tick(latest - clock.now);
		},

		// flush lets every already-resolved promise continue, without advancing time.
		flush,
	};
	return clock;
}

// flush drains the microtask queue by yielding to the event loop once.
function flush() {
	return new Promise((resolve) => setImmediate(resolve));
}
