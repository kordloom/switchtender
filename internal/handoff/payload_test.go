package handoff

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestAPayloadPrintsNoSecret pins that a payload handed to a logger or a format verb by mistake
// prints the run, the credential ids, and the answer names, and no value, token, or answer. The
// values travel only inside the seal.
func TestAPayloadPrintsNoSecret(t *testing.T) {
	t.Parallel()
	p := testPayload("run_alpha")
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	c := p.Credential("cred_ssh")
	renderings := map[string]string{
		"json":                string(encoded),
		"%v":                  fmt.Sprintf("%v", p),
		"%+v":                 fmt.Sprintf("%+v", p),
		"%#v":                 fmt.Sprintf("%#v", p),
		"String":              p.String(),
		"%v of a value":       fmt.Sprintf("%v", *p),
		"credential %+v":      fmt.Sprintf("%+v", c),
		"credential %#v":      fmt.Sprintf("%#v", *c),
		"credentials %+v":     fmt.Sprintf("%+v", p.Credentials),
		"credential in a map": fmt.Sprintf("%v", map[string]*Credential{"k": c}),
	}
	for how, text := range renderings {
		for _, secret := range secretsOf() {
			if strings.Contains(text, secret) {
				t.Errorf("rendered with %s, the payload shows %q: %s", how, secret, text)
			}
		}
	}
	if !strings.Contains(renderings["%v"], "cred_ssh") ||
		!strings.Contains(renderings["%v"], "db_password") {
		t.Errorf("the rendering %q does not say what the payload carries", renderings["%v"])
	}
}

// TestPayloadWipeDropsEveryValue pins that a wiped payload references no secret, so a payload kept
// past its use by mistake holds nothing.
func TestPayloadWipeDropsEveryValue(t *testing.T) {
	t.Parallel()
	p := testPayload("run_alpha")
	creds := append([]*Credential(nil), p.Credentials...)
	p.Wipe()
	if !p.Empty() || p.CredentialIDs != nil || p.Types != nil || p.Answers != nil {
		t.Errorf("Wipe() left the payload holding something: %+v", p)
	}
	for _, c := range creds {
		if c.Value != "" || c.Token != "" || len(c.Record.Settings) != 0 {
			t.Errorf("Wipe() left credential %s holding its value", c.Record.ID)
		}
	}
	var none *Payload
	none.Wipe()
	if !none.Empty() {
		t.Errorf("a nil payload is not empty")
	}
}

// TestPayloadDescribesWhatItCarries pins the lookups the executor and the audit record read: by id,
// by type, and the sorted lists the chain records.
func TestPayloadDescribesWhatItCarries(t *testing.T) {
	t.Parallel()
	p := testPayload("run_alpha")
	p.AddType(&credential.CredentialType{ID: "ctype_a", Name: "duplicate"})
	p.AddType(&credential.CredentialType{ID: "ctype_b", Name: "second"})
	p.AddType(nil)
	got := struct {
		IDs, Answers, Types []string
		Missing             bool
		Found               string
	}{
		IDs: p.DeliveredIDs(), Answers: p.AnswerNames(),
		Missing: p.Credential("cred_absent") == nil && p.Type("ctype_absent") == nil,
		Found:   p.Type("ctype_a").Name,
	}
	for _, typ := range p.Types {
		got.Types = append(got.Types, typ.ID)
	}
	want := struct {
		IDs, Answers, Types []string
		Missing             bool
		Found               string
	}{
		IDs: []string{"cred_oidc", "cred_ssh"}, Answers: []string{"db_password"},
		Types: []string{"ctype_a", "ctype_b"}, Missing: true, Found: "Datadog",
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("payload description mismatch (-want +got):\n%s", diff)
	}
	var none *Payload
	if none.Credential("x") != nil || none.Type("x") != nil || none.DeliveredIDs() != nil ||
		none.AnswerNames() != nil {
		t.Errorf("a nil payload answered a lookup")
	}
}
