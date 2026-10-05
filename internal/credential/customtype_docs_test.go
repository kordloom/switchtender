package credential

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docBlock is one indented code block of a Markdown page and the heading it sits under.
type docBlock struct {
	// Heading is the text of the nearest heading above the block.
	Heading string
	// Text is the block with its indentation removed.
	Text string
}

// secretsPageBlocks returns the indented code blocks of the secrets page in order.
func secretsPageBlocks(t *testing.T) []docBlock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "secrets.md"))
	if err != nil {
		t.Fatalf("read the secrets page: %v", err)
	}
	var blocks []docBlock
	var heading string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, docBlock{Heading: heading,
				Text: strings.TrimSpace(strings.Join(cur, "\n"))})
			cur = nil
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "    "):
			cur = append(cur, strings.TrimPrefix(line, "    "))
		case strings.TrimSpace(line) == "" && len(cur) > 0:
			cur = append(cur, "")
		default:
			flush()
			if strings.HasPrefix(line, "#") {
				heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
			}
		}
	}
	flush()
	return blocks
}

// TestTheSecretsPageTypeExamplesAreTrue holds the credential type examples on the secrets page to
// this build. Every type the page defines validates as one created here, except the one the page
// shows being refused, and the refusal the page prints is the one Validate gives, word for word.
// The kubeconfig example is a type the import report would name for the switch.
func TestTheSecretsPageTypeExamplesAreTrue(t *testing.T) {
	t.Parallel()
	blocks := secretsPageBlocks(t)
	var defined, refused, kube int
	for i, b := range blocks {
		body, posted := strings.CutPrefix(b.Text, "POST /v1/credential-types\n")
		isKube := b.Heading == "Kubeconfig types from AWX" && strings.HasPrefix(b.Text, "{")
		if !posted && !isKube {
			continue
		}
		var typ CredentialType
		if err := json.Unmarshal([]byte(body), &typ); err != nil {
			t.Fatalf("an example under %q is not a type: %v\n%s", b.Heading, err, body)
		}
		err := typ.Validate()
		if i+1 < len(blocks) && strings.HasPrefix(blocks[i+1].Text, "invalid credential type:") {
			refused++
			want := strings.Join(strings.Fields(blocks[i+1].Text), " ")
			if err == nil || strings.Join(strings.Fields(err.Error()), " ") != want {
				t.Errorf("the page prints a refusal this build does not give:\npage:  %s\nbuild: %v",
					want, err)
			}
			continue
		}
		defined++
		if err != nil {
			t.Errorf("an example under %q does not validate: %v\n%s", b.Heading, err, body)
		}
		if isKube {
			kube++
			if _, ok := typ.KubeconfigShaped(); !ok {
				t.Errorf("the kubeconfig example is not one the import names for the switch:\n%s",
					body)
			}
		}
	}
	if defined < 4 || refused != 1 || kube != 1 {
		t.Errorf("found %d valid examples, %d refusals, and %d kubeconfig examples, want at least "+
			"4, exactly 1, and exactly 1", defined, refused, kube)
	}
}
