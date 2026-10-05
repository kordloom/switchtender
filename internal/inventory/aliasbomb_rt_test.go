package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestResolveNativeRefusesAnAliasBomb pins that the native engine bounds how far a YAML inventory's
// aliases expand, so a sub-kilobyte document cannot make it allocate without end.
//
// loadYAML decodes into a yaml.Node tree, where an alias stays a single AliasNode pointing at its
// anchor, and yamlBuilder.value materializes the tree by re-walking each aliased anchor on every
// reference. Decoding into a node tree bypasses go-yaml's own alias budget, and real
// ansible-inventory and PyYAML do not bound this either, so a classic billion-laughs document, a
// handful of ten-wide anchors stacked a few deep, expands to 10^depth values and hangs every engine.
// ResolveNative is reached from the provisioning callback handler, the doctor sweep, and
// constructed-inventory composition, so without a bound a tiny stored inventory hangs the server.
//
// The fix counts constructed values against a budget scaled to the document's size and refuses the
// document past it, since only alias expansion makes values far outnumber bytes.
func TestResolveNativeRefusesAnAliasBomb(t *testing.T) {
	t.Parallel()
	const width, levels = 10, 6 // 10^7 values from well under a kilobyte, which hangs today.
	var sb strings.Builder
	sb.WriteString("all:\n  vars:\n    boom:\n")
	sb.WriteString("      a0: &a0 [")
	for i := 0; i < width; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\"lol\"")
	}
	sb.WriteString("]\n")
	for lvl := 1; lvl <= levels; lvl++ {
		fmt.Fprintf(&sb, "      a%d: &a%d [", lvl, lvl)
		for i := 0; i < width; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "*a%d", lvl-1)
		}
		sb.WriteString("]\n")
	}
	doc := sb.String()
	if len(doc) > 1024 {
		t.Fatalf("test document is %d bytes, meant to be a sub-kilobyte bomb", len(doc))
	}

	done := make(chan error, 1)
	go func() {
		_, err := ResolveNative(doc)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidInventory) {
			t.Fatalf("ResolveNative(alias bomb) error = %v, want ErrInvalidInventory so a %d-byte "+
				"document cannot expand without bound", err, len(doc))
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("ResolveNative did not finish resolving a %d-byte alias bomb in 20s", len(doc))
	}
}

// TestResolveNativeStillReadsAModestlyAliasedInventory is the negative control: the bound must not
// refuse an ordinary inventory that uses an anchor and a merge key, the way a real fleet shares
// variables across hosts. Both hosts take the anchored variables, and the document resolves.
func TestResolveNativeStillReadsAModestlyAliasedInventory(t *testing.T) {
	t.Parallel()
	doc := "all:\n" +
		"  children:\n" +
		"    web:\n" +
		"      vars: &common\n" +
		"        ansible_user: deploy\n" +
		"        http_port: 8080\n" +
		"      hosts:\n" +
		"        h1:\n" +
		"          <<: *common\n" +
		"        h2:\n" +
		"          <<: *common\n"
	listing, err := ResolveNative(doc)
	if err != nil {
		t.Fatalf("ResolveNative(modestly aliased inventory) error = %v, want it resolved", err)
	}
	for _, host := range []string{"h1", "h2"} {
		vars := listing.Vars(host)
		if vars["ansible_user"] != "deploy" {
			t.Errorf("host %s vars = %v, want the anchored ansible_user merged in", host, vars)
		}
	}
}
