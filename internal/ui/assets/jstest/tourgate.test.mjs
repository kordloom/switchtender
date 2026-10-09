// Tests for what the welcome tour says about the approval gate. An agent can never approve on any
// edition, and only the rule that refuses the requester's own approval is Team's, so the step on
// separation of duties states the agent's hold without a tier, keeps the agent out of its Team
// clause, and gives Community its documented ceiling of one rule.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// gateStep returns the welcome tour's step on separation of duties.
function gateStep(app) {
	const welcome = app.TOURS.find((t) => t.id === "welcome");
	assert.ok(welcome, "there is no welcome tour");
	const step = welcome.steps.find((s) => s.title === "Separation of duties");
	assert.ok(step, "the welcome tour has no step on separation of duties");
	return step;
}

// sentencesOf splits a step's body at sentence ends.
function sentencesOf(body) {
	return body.split(/(?<=\.)\s+/).filter((s) => s.length > 0);
}

test("the tour keeps the agent's hold out of the Team clause", () => {
	const { app } = loadPage("overview", { quiet: true });
	const sentences = sentencesOf(gateStep(app).body);
	for (const sentence of sentences) {
		if (!/\bTeam\b/.test(sentence)) continue;
		assert.doesNotMatch(sentence, /\bagents?\b/i,
			"a Team clause names the agent, which makes its hold read as a paid feature: " + sentence);
	}
});

test("the tour states the agent's hold without a tier and names Community's ceiling", () => {
	const { app } = loadPage("overview", { quiet: true });
	const sentences = sentencesOf(gateStep(app).body);
	assert.ok(sentences.some((s) => /agent can never approve/i.test(s) && !/\bTeam\b/.test(s)),
		"no sentence says an agent can never approve, on any edition: " + sentences.join(" "));
	assert.ok(sentences.some((s) => /\bCommunity\b/.test(s) && /one rule/.test(s)),
		"no sentence gives Community its documented ceiling of one rule: " + sentences.join(" "));
});
