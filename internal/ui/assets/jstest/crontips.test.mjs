import test from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

// wireCronTips runs again every time the schedules page rewires after a save, but the form input it
// subscribes outlives the table being rewired. Without a guard each rewire added another listener to
// the same field, so after ten saves one keystroke did the same work ten times.
test("wireCronTips subscribes an input once however often it runs", () => {
	const app = loadParts(["01-boot.js", "17-cron.js"]);
	let listeners = 0;
	const input = {
		value: "0 9 * * *",
		dataset: {},
		addEventListener() { listeners++; },
	};
	const root = {
		querySelectorAll: (sel) => (sel.includes("data-cron-input") ? [input] : []),
	};

	app.wireCronTips(root);
	assert.equal(listeners, 1, "the first call must subscribe the input");
	const firstTip = input.dataset.tip;
	assert.ok(firstTip, "the tip is written on the first call");

	app.wireCronTips(root);
	app.wireCronTips(root);
	assert.equal(listeners, 1, "rewiring must not stack another listener on the same input");
});

// The tip itself must still refresh on every call, or a rewire after a save would leave the field
// explaining the value it held before the save.
test("wireCronTips refreshes the tip on every run, including ones it does not resubscribe", () => {
	const app = loadParts(["01-boot.js", "17-cron.js"]);
	const input = { value: "0 9 * * *", dataset: {}, addEventListener() {} };
	const root = { querySelectorAll: (sel) => (sel.includes("data-cron-input") ? [input] : []) };

	app.wireCronTips(root);
	const before = input.dataset.tip;
	input.value = "*/5 * * * *";
	app.wireCronTips(root);
	assert.notEqual(input.dataset.tip, before, "the tip must follow the current value");
});
