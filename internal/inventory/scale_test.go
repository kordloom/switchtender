package inventory

import (
	"fmt"
	"strings"
	"testing"
)

// TestRedactLargeInventory proves the secret masker holds on an inventory of thousands of hosts, the
// fan-out the product is built for. Redaction walks the whole content, and every run serves a redacted
// copy and hands the collected secrets to the run-log masker, so a host list this size that stalled or
// missed a value would leak a credential or hang an API request. This uses a distinct password per host,
// which is the worst case for both the line scan and the collected-secret set.
func TestRedactLargeInventory(t *testing.T) {
	t.Parallel()
	const hosts = 5000

	var b strings.Builder
	b.WriteString("[web]\n")
	for i := range hosts {
		// A real host line: a non-secret address and role around a unique inline password.
		fmt.Fprintf(&b, "web%04d ansible_host=10.%d.%d.%d ansible_password=secret%04d role=frontend\n",
			i, i/65536%256, i/256%256, i%256, i)
	}
	content := b.String()

	redacted := Redact(content)

	// Every password is masked, and none of the plaintext survives.
	if got := strings.Count(redacted, "ansible_password="+RedactedValue); got != hosts {
		t.Errorf("masked %d of %d passwords", got, hosts)
	}
	for _, plain := range []string{"secret0000", "secret2500", "secret4999"} {
		if strings.Contains(redacted, plain) {
			t.Errorf("plaintext %q survived redaction", plain)
		}
	}
	// The non-secret layout is preserved, so the inventory still reads as one.
	for _, keep := range []string{"web0000", "web4999", "ansible_host=10.", "role=frontend"} {
		if !strings.Contains(redacted, keep) {
			t.Errorf("redaction dropped non-secret content %q", keep)
		}
	}

	// The masker's list holds every distinct password so a run's log cannot leak one, deduped.
	secrets := Secrets(content)
	if len(secrets) != hosts {
		t.Errorf("collected %d distinct secrets, want %d", len(secrets), hosts)
	}
}
