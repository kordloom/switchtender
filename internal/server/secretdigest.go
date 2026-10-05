package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/template"
)

// secretWithheldMarker stands in for a secret the gate keeps out of a request's fingerprint. It is
// fixed, so the entry still commits that the field was sent, and it is not a value anybody types.
const secretWithheldMarker = "«withheld: secret»"

// secretSurveyVarsFunc returns the variables a template's survey asks as secret questions. ok is
// false when the template cannot be read, and the gate then withholds every answer the launch
// carries rather than guess which of them are secret.
type secretSurveyVarsFunc func(ctx context.Context, templateID string) (vars map[string]bool, ok bool)

// templateSecretVars returns the secret survey variable lookup over a template store, nil without
// one. The lookup reads the template only to decide what the gate withholds, so it shows the caller
// nothing and needs no grant of its own.
func templateSecretVars(store template.Store) secretSurveyVarsFunc {
	if store == nil {
		return nil
	}
	return func(ctx context.Context, templateID string) (map[string]bool, bool) {
		t, err := store.Get(ctx, templateID)
		if err != nil {
			return nil, false
		}
		vars := map[string]bool{}
		for _, f := range t.Survey {
			if f.Secret() {
				vars[f.Var] = true
			}
		}
		return vars, true
	}
}

// withheldBody returns the bytes the gate digests for a request body: the body with every value
// that must not be committed replaced by a fixed marker.
//
// The request-body fingerprint is a keyed commitment whose key is stored beside the entry, so anyone
// holding the database can confirm a guess of what it committed. That is the right shape for a
// change's configuration and the wrong one for a secret, which may be short enough to guess. A
// secret under a name the redaction recognizes, a password or a token, is already masked inside the
// digest. These are the secrets whose names say nothing: the answer to a secret survey question, a
// secret question's default, and a notification target's address and key, where the address is
// the credential for most channels. A decision's free text is withheld on its own terms, since the
// decision entry commits it. A body with nothing to withhold is digested exactly as sent.
func (g *authGate) withheldBody(r *http.Request, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	if decisionTextRoute(r.URL.Path) {
		return withholdReasonText(body)
	}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/v1"),
		"/"), "/")
	switch {
	case r.Method == http.MethodPost && len(segments) == 3 && segments[0] == "templates" &&
		segments[2] == "launch":
		secret, known := g.secretVarsOf(r.Context(), segments[1])
		return withholdJSON(body, func(doc map[string]any) bool {
			return withholdAnswers(doc, secret, known)
		})
	case segments[0] == "templates" && (r.Method == http.MethodPost && len(segments) == 1 ||
		(r.Method == http.MethodPut || r.Method == http.MethodPatch) && len(segments) == 2):
		return withholdJSON(body, func(doc map[string]any) bool {
			defaults := withholdSurveyDefaults(doc["survey"])
			targets := withholdTargetList(doc["notifications"])
			return defaults || targets
		})
	case segments[0] == "runs" && r.Method == http.MethodPost && len(segments) == 1:
		return withholdJSON(body, func(doc map[string]any) bool {
			return withholdTargetList(doc["notifications"])
		})
	case segments[0] == "notifications" && (r.Method == http.MethodPost && len(segments) == 1 ||
		(r.Method == http.MethodPut || r.Method == http.MethodPatch) && len(segments) == 2):
		return withholdJSON(body, withholdTarget)
	}
	return body
}

// secretVarsOf returns the secret survey variables of the template a launch names, and whether they
// are known.
func (g *authGate) secretVarsOf(ctx context.Context, templateID string) (map[string]bool, bool) {
	if g.secretVars == nil {
		return nil, false
	}
	return g.secretVars(ctx, templateID)
}

// withholdJSON applies withhold to body read as one JSON object and returns the result, or body
// unchanged when withhold found nothing to replace. A body that is not a single JSON object is
// refused by every handler these routes reach, and is digested as the marker alone rather than
// committed raw, since what it holds cannot be read to withhold anything from it.
func withholdJSON(body []byte, withhold func(doc map[string]any) bool) []byte {
	var doc map[string]any
	if !json.Valid(body) || strictDecode(bytes.NewReader(body), &doc) != nil || doc == nil {
		return []byte(secretWithheldMarker)
	}
	if !withhold(doc) {
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return []byte(secretWithheldMarker)
	}
	return out
}

// withholdValue replaces m[key] with the marker when it holds anything, reporting whether it did.
func withholdValue(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil || v == "" {
		return false
	}
	m[key] = secretWithheldMarker
	return true
}

// withholdAnswers withholds a launch's answers to the template's secret questions, and any extra
// var of the same name, which the launch handler refuses only after the gate has recorded it. When
// the questions are not known every answer is withheld.
func withholdAnswers(doc map[string]any, secret map[string]bool, known bool) bool {
	changed := false
	if answers, ok := doc["answers"].(map[string]any); ok {
		for name := range answers {
			if !known || secret[name] {
				changed = withholdValue(answers, name) || changed
			}
		}
	} else if !known {
		changed = withholdValue(doc, "answers") || changed
	}
	if vars, ok := doc["extra_vars"].(map[string]any); ok {
		for name := range vars {
			if secret[name] {
				changed = withholdValue(vars, name) || changed
			}
		}
	}
	return changed
}

// withholdSurveyDefaults withholds the default of every survey question that collects a secret, and
// of every question whose type cannot be read, since its default may be one.
func withholdSurveyDefaults(survey any) bool {
	fields, ok := survey.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range fields {
		f, ok := item.(map[string]any)
		if !ok || !mayBeSecretField(f["type"]) {
			continue
		}
		changed = withholdValue(f, "default") || changed
	}
	return changed
}

// mayBeSecretField reports whether a survey question's type, as sent, collects a secret or is not a
// type the gate can read.
func mayBeSecretField(typ any) bool {
	if typ == nil {
		return false
	}
	name, ok := typ.(string)
	if !ok {
		return true
	}
	// The type is read the way the template reads it, so an alias such as AWX's password is the
	// secret type here too.
	var ft template.FieldType
	if ft.UnmarshalJSON([]byte(strconv.Quote(name))) != nil {
		return true
	}
	switch ft {
	case "", template.FieldText, template.FieldMultiline, template.FieldInt, template.FieldBool,
		template.FieldChoice:
		return false
	}
	return true
}

// withholdTargetList withholds the address and key of every notification target in a list.
func withholdTargetList(targets any) bool {
	list, ok := targets.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range list {
		if t, ok := item.(map[string]any); ok {
			changed = withholdTarget(t) || changed
		}
	}
	return changed
}

// withholdTarget withholds a notification target's address and key. The address of a webhook,
// Slack, Teams, Discord, Mattermost, Rocket.Chat, or ntfy target is what authorizes a post to it,
// and the key is a PagerDuty routing key or a Grafana token.
func withholdTarget(t map[string]any) bool {
	address := withholdValue(t, "url")
	key := withholdValue(t, "key")
	return address || key
}
