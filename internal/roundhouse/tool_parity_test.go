package roundhouse

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestEveryBuiltinToolClassifiesAndDispatches walks run's canonical tool set against this
// package's classifier and router, so the set cannot fork.
//
// The set used to be restated here three times: the classifier, the override guard, and the
// dispatch switch. run.ValidTool gated submission on its copy while execution read these, so a
// tool added to one and not the other passed validation and then died at dispatch, or classified
// as an extension and skipped the container path. The classifier and the override guard now
// consume run's set directly; the dispatch switch is the one restatement left, and this holds it:
// every canonical tool must route to a real runner, not fall through to the extension map.
func TestEveryBuiltinToolClassifiesAndDispatches(t *testing.T) {
	t.Parallel()
	router := newToolRouter(false, "", "", false, ContainerLimits{})
	for _, tool := range run.BuiltinTools() {
		if !isBuiltinTool(tool) {
			t.Errorf("run.BuiltinTools lists %q but isBuiltinTool denies it: the container path "+
				"treats a built-in as an extension", tool)
		}
		// A canonical tool must dispatch to its own runner. An unrouted tool falls through to the
		// extension map and returns the unknown-tool error, which is exactly the fork this guards.
		_, err := router.Run(context.Background(), Spec{Tool: tool, Command: ""}, io.Discard)
		if errors.Is(err, ErrUnknownTool) {
			t.Errorf("the router does not dispatch the built-in tool %q: submission would accept "+
				"a run the executor cannot run", tool)
		}
	}
}
