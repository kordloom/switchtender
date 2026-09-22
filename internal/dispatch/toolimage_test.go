package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAHostToolNeverRecordsAnImageItDidNotEnter covers the evidence a signed record makes about
// where a run executed.
//
// The container runner executes the compiled-in tools. A tool registered through the SDK or a plugin
// is run on the host by design, and the router passes it through rather than containerizing it. The
// image was still stamped onto the run afterward as "the image the run actually executed in", and
// that value is persisted in the terminal write, committed into the tamper-evident outcome, and
// printed in the exported dossier. So for every plugin-tool run the evidence asserted a container
// environment the run never entered, which is the one thing the evidence exists to be right about.
//
// Both ends are closed. A request naming an image for such a tool is refused where an operator can
// see it, and a default image applied by the server or a project is not recorded on a run it does
// not apply to, so the record stays truthful even when nobody asked for an image at all.
func TestAHostToolNeverRecordsAnImageItDidNotEnter(t *testing.T) {
	t.Parallel()
	const hostTool = "hello-from-a-plugin"
	// Registered the way an extension registers one: the name so submissions are accepted, and the
	// runner so something executes it. Neither makes it containerizable, which is the point.
	run.RegisterTool(hostTool)
	roundhouse.RegisterRunner(hostTool, roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}))

	ctx := context.Background()

	// A request that names an image for a tool that cannot enter one is refused, by name.
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(),
		WithClaimGate(func() error { return errNoClaimingInThisTest }))
	defer d.Close()
	_, err := d.Submit(ctx, "", "inv", run.WithTool(hostTool), run.WithCommand("id"),
		run.WithImage("alpine@sha256:deadbeef", ""))
	if !errors.Is(err, ErrToolImage) {
		t.Errorf("submitting a plugin tool with an image = %v, want %v: the run would execute on "+
			"the host while its signed record named a container", err, ErrToolImage)
	}

	// And a server-wide default image, which the request never asked for, is not recorded on one.
	defaulted := run.NewMemStore()
	dd := New(defaulted, okRunner(), nil, WithNoJanitor(),
		WithDefaultImage("alpine@sha256:deadbeef"))
	defer dd.Close()
	created, err := dd.Submit(ctx, "", "inv", run.WithTool(hostTool), run.WithCommand("id"))
	if err != nil {
		t.Fatalf("Submit() error = %v: a plugin tool with no image is an ordinary run", err)
	}
	waitFor(t, func() bool {
		got, gerr := defaulted.Get(ctx, created.ID)
		return gerr == nil && got.Status.Terminal()
	})
	got, err := defaulted.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Image != "" {
		t.Errorf("a plugin-tool run recorded the image %q, which it never entered: the tool runs "+
			"on the host and the record is what an auditor reads to learn where it ran", got.Image)
	}
}
