// Tests for the wording a read-only server's pages use, which depends on whether it is the demo.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// TestReadOnlyWordsItselfForWhatItIs pins the phrase a disabled control uses. A server started with
// --read-only is somebody's install, and its controls said they were disabled in the demo.
test("a read-only install's controls do not say they are in the demo", () => {
	for (const [demo, want] of [[false, "Disabled on this read-only server"], [true, "Disabled in this read-only demo"]]) {
		const { app, document } = loadPage("runs", { vars: { ReadOnly: true, Demo: demo }, quiet: true });
		assert.equal(document.body.dataset.readonly, "true");
		assert.equal(app.readOnlyReason(), want, "demo=" + demo);
	}
});
