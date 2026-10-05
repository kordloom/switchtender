package handoff

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/kordloom/switchtender/internal/credential"
)

// Payload is one run's secrets as the control node opened them for the worker that claimed it.
//
// Every secret field is tagged out of JSON. The package seals a private wire form of the payload,
// so a Payload that is printed, logged, or marshaled by mistake carries the run, the credential
// ids, and their kinds, and none of the values.
type Payload struct {
	// RunID is the run the secrets were opened for.
	RunID string `json:"run_id"`
	// DryRun is the execution mode the run's identity tokens were minted for, true for a plan. A
	// worker executing in the other mode refuses them, so a token minted to plan is never used to
	// apply, and the reverse.
	DryRun bool `json:"dry_run,omitempty"`
	// CredentialIDs lists the credentials the run materializes, in the order it materializes them.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// Credentials holds every opened credential: the run's own, its inventory's, and the registry
	// login it pulls its image with.
	Credentials []*Credential `json:"credentials,omitempty"`
	// Types holds the custom credential types the typed credentials name. A type is a definition and
	// carries no secret, but the worker cannot apply a typed credential without it.
	Types []*credential.CredentialType `json:"types,omitempty"`
	// Answers holds the opened secret survey answers by variable name.
	Answers map[string]string `json:"-"`
	// Inventory is the run's opened inventory snapshot, the content materialized when the run was
	// submitted. It can carry an ansible_password or an API token, so it travels sealed like any
	// secret.
	Inventory string `json:"-"`
	// PlanFile is the opened plan file a gated terraform or opentofu apply carries out. A plan file
	// holds the values the configuration was planned with, sensitive ones included.
	PlanFile []byte `json:"-"`
}

// Credential is one opened credential.
type Credential struct {
	// Record is the credential as stored, less its sealed material and its source: its id, name,
	// kind, type, vault id, and non-secret settings, which is what applying it needs.
	Record credential.Credential `json:"record"`
	// Value is the resolved secret. It is empty for a federated credential, which stores none.
	Value string `json:"-"`
	// Token is the identity token minted for a federated credential, empty for any other.
	Token string `json:"-"`
}

// NewCredential returns the opened form of c with its resolved value. Only the fields applying a
// credential reads are copied: the sealed secret never leaves the control node, and the source is
// dropped because the value has already been resolved through it.
func NewCredential(c *credential.Credential, value string) *Credential {
	return &Credential{
		Record: credential.Credential{
			ID: c.ID, Name: c.Name, Kind: c.Kind, TypeID: c.TypeID, VaultID: c.VaultID,
			Settings: maps.Clone(c.Settings),
		},
		Value: value,
	}
}

// Credential returns the opened credential with the given id, or nil.
func (p *Payload) Credential(id string) *Credential {
	if p == nil {
		return nil
	}
	for _, c := range p.Credentials {
		if c != nil && c.Record.ID == id {
			return c
		}
	}
	return nil
}

// Type returns the custom credential type with the given id, or nil.
func (p *Payload) Type(id string) *credential.CredentialType {
	if p == nil {
		return nil
	}
	for _, t := range p.Types {
		if t != nil && t.ID == id {
			return t
		}
	}
	return nil
}

// AddType records a custom credential type a typed credential names, once.
func (p *Payload) AddType(t *credential.CredentialType) {
	if t == nil || p.Type(t.ID) != nil {
		return
	}
	p.Types = append(p.Types, t)
}

// DeliveredIDs returns the id of every credential the payload carries, sorted, for the record of
// what a worker received.
func (p *Payload) DeliveredIDs() []string {
	if p == nil {
		return nil
	}
	ids := make([]string, 0, len(p.Credentials))
	for _, c := range p.Credentials {
		if c != nil && !slices.Contains(ids, c.Record.ID) {
			ids = append(ids, c.Record.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// AnswerNames returns the variable name of every secret answer the payload carries, sorted.
func (p *Payload) AnswerNames() []string {
	if p == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(p.Answers))
}

// Empty reports whether the payload carries nothing to deliver.
func (p *Payload) Empty() bool {
	return p == nil || (len(p.Credentials) == 0 && len(p.Answers) == 0 && p.Inventory == "" &&
		len(p.PlanFile) == 0)
}

// String describes the credential by id and kind and never by value, so a credential passed to a
// format verb by mistake prints nothing secret.
func (c Credential) String() string {
	return fmt.Sprintf("credential %s (%s)", c.Record.ID, c.Record.Kind)
}

// GoString is String, for the %#v verb, which would otherwise print every field.
func (c Credential) GoString() string { return c.String() }

// String describes the payload by run and by what it carries, never by value.
func (p Payload) String() string {
	return fmt.Sprintf("secrets for %s: credentials %v, answers %v, inventory snapshot %v, plan "+
		"file %v", p.RunID, (&p).DeliveredIDs(), (&p).AnswerNames(), p.Inventory != "",
		len(p.PlanFile) > 0)
}

// GoString is String, for the %#v verb.
func (p Payload) GoString() string { return p.String() }

// Wipe drops every value the payload holds, so nothing secret stays reachable through it once the
// executor has written what it needed. Go cannot overwrite a string in place, so what is dropped is
// the reference: the memory goes back to the runtime, which is why the bytes the payload was
// decoded from are zeroed as soon as decoding finishes.
func (p *Payload) Wipe() {
	if p == nil {
		return
	}
	for _, c := range p.Credentials {
		if c != nil {
			c.Value, c.Token = "", ""
			clear(c.Record.Settings)
		}
	}
	clear(p.Credentials)
	p.Credentials = nil
	clear(p.Answers)
	p.Answers = nil
	p.Types = nil
	p.CredentialIDs = nil
	p.Inventory = ""
	clear(p.PlanFile)
	p.PlanFile = nil
}

// wirePayload is the form a payload is sealed in. It exists so the secret fields can carry JSON
// tags here, inside the seal, while the Payload type a caller handles keeps them tagged out.
type wirePayload struct {
	// RunID is the run the secrets were opened for.
	RunID string `json:"run_id"`
	// DryRun is the execution mode identity tokens were minted for.
	DryRun bool `json:"dry_run,omitempty"`
	// CredentialIDs lists the credentials in materialization order.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// Credentials holds each opened credential with its value.
	Credentials []wireCredential `json:"credentials,omitempty"`
	// Types holds the custom types the typed credentials name.
	Types []*credential.CredentialType `json:"types,omitempty"`
	// Answers holds the secret survey answers by variable name.
	Answers map[string]string `json:"answers,omitempty"`
	// Inventory is the run's inventory snapshot.
	Inventory string `json:"inventory,omitempty"`
	// PlanFile is the plan file a gated apply carries out.
	PlanFile []byte `json:"plan_file,omitempty"`
}

// wireCredential is one credential inside the seal.
type wireCredential struct {
	// Record is the credential with no sealed material.
	Record credential.Credential `json:"record"`
	// Value is the resolved secret.
	Value string `json:"value,omitempty"`
	// Token is a federated credential's identity token.
	Token string `json:"token,omitempty"`
}

// marshalPayload encodes p in its wire form, for sealing.
func marshalPayload(p *Payload) ([]byte, error) {
	w := wirePayload{
		RunID: p.RunID, DryRun: p.DryRun, CredentialIDs: p.CredentialIDs, Types: p.Types,
		Answers: p.Answers, Inventory: p.Inventory, PlanFile: p.PlanFile,
	}
	for _, c := range p.Credentials {
		if c == nil {
			continue
		}
		w.Credentials = append(w.Credentials, wireCredential{Record: c.Record, Value: c.Value,
			Token: c.Token})
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("%w: encode payload: %w", ErrSeal, err)
	}
	return raw, nil
}

// unmarshalPayload decodes a payload from its wire form, after opening.
func unmarshalPayload(raw []byte) (*Payload, error) {
	var w wirePayload
	if err := json.Unmarshal(raw, &w); err != nil {
		// The decoder's message can quote the bytes it stopped at, which are secret, so only the
		// fact of the failure is reported.
		return nil, fmt.Errorf("%w: the opened payload does not decode", ErrEnvelope)
	}
	p := &Payload{
		RunID: w.RunID, DryRun: w.DryRun, CredentialIDs: w.CredentialIDs, Types: w.Types,
		Answers: w.Answers, Inventory: w.Inventory, PlanFile: w.PlanFile,
	}
	for _, c := range w.Credentials {
		p.Credentials = append(p.Credentials, &Credential{Record: c.Record, Value: c.Value,
			Token: c.Token})
	}
	return p, nil
}
