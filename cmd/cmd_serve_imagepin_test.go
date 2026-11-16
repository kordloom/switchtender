package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/run"
)

// TestServeRefusesAnUnpinnedImageUnlessTheFlagAllowsIt builds a dispatcher from the options the
// serve flags select and submits a run whose registry does not answer. With every flag at its
// default the submission is refused, so an approval always covers a digest. With
// --allow-unpinned-image the run is accepted, bound to its tag.
//
// The image names this machine's loopback interface, which the server's resolver refuses to dial,
// so the lookup fails the way an unreachable registry does and nothing leaves the test.
//
// The test is serial: it sets a serve flag, which is a package-level variable.
func TestServeRefusesAnUnpinnedImageUnlessTheFlagAllowsIt(t *testing.T) {
	const image = "127.0.0.1:9/acme/runner:1"
	flag := serveCmd.Flags().Lookup("allow-unpinned-image")
	if flag == nil {
		t.Fatal("serve has no --allow-unpinned-image flag")
	}
	tests := []struct {
		// Set is the value --allow-unpinned-image is given, empty to leave it at its default.
		Set string
		// WantImage is the image the accepted run carries.
		WantImage string
		// Want is the error the submission ends in.
		Want error
	}{{ // Test 0: With the flag at its default the submission is refused.
		Want: dispatch.ErrImageUnpinned,
	}, { // Test 1: With the flag given the run is accepted, bound to its tag.
		Set: "true", WantImage: image,
	}, { // Test 2: The flag given as false refuses, as the default does.
		Set: "false", Want: dispatch.ErrImageUnpinned,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: every row sets the same flag.
			if got := flag.Value.String(); got != flag.DefValue {
				t.Fatalf("--allow-unpinned-image = %s before this row set it, want its default %s",
					got, flag.DefValue)
			}
			if test.Set != "" {
				t.Cleanup(func() { _ = flag.Value.Set(flag.DefValue) })
				if err := flag.Value.Set(test.Set); err != nil {
					t.Fatalf("set --allow-unpinned-image to %s: %v", test.Set, err)
				}
			}
			ctx := context.Background()
			store := run.NewMemStore()
			d := dispatch.New(store, dispatchtest.OKRunner(), nil, imageResolverOption(),
				unpinnedImageOption(), dispatch.WithNoJanitor(),
				dispatch.WithClaimGate(func() error { return errors.New("this test claims nothing") }))
			t.Cleanup(d.Close)
			r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("true"),
				run.WithImage(image, ""))
			if !errors.Is(err, test.Want) {
				t.Fatalf("Submit() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), "the registry did not answer") {
					t.Errorf("refusal %q does not say the registry did not answer", err)
				}
				if runs, lerr := store.List(ctx); lerr != nil || len(runs) != 0 {
					t.Errorf("stored runs = %d (%v), want none for a refused submission",
						len(runs), lerr)
				}
				return
			}
			if r.Image != test.WantImage {
				t.Errorf("accepted image = %q, want the tag %q", r.Image, test.WantImage)
			}
		})
	}
}
