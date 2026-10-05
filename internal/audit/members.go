package audit

import (
	"fmt"
	"sort"

	"github.com/kordloom/loomseal/jcs"
)

// The exact member set each object in a bundle may carry, mirroring the LoomSeal schema's
// additionalProperties: false. A member outside its set, including a case variant of a known member,
// is refused rather than folded onto a struct field. encoding/json matches a JSON member to a field
// case-insensitively, so without this a claim carrying both at and At, or a producer carrying
// install_id and Install_ID, would decode the case variant into the field a verdict reads while a
// reader and the leaf saw the exact member, and the two could differ. Two surfaces stay open and are
// not listed: a claim payload, whose members belong to the product, and chain.params.
var (
	bundleMembers = memberSet("anchors", "attestations", "bundle_id", "chain", "claims", "created_at",
		"loomseal", "producer", "signatures", "subject")
	producerMembers    = memberSet("install_id", "key_id", "product", "product_version", "public_key")
	subjectMembers     = memberSet("id", "type")
	chainMembers       = memberSet("consistency", "head", "keyed", "params", "profile")
	consistencyMembers = memberSet("from_root", "from_size", "path")
	coordsMembers      = memberSet("link", "prev", "seq")
	claimMembers       = memberSet("at", "attestations", "chain", "disclosures", "evidence",
		"inclusion", "payload", "type", "verdict")
	inclusionMembers = memberSet("path")
	evidenceMembers  = memberSet("digest", "location", "media_type", "present", "role")
	verdictMembers   = memberSet("decision", "detail", "inputs_digest", "policy", "policy_digest")
	anchorMembers    = memberSet("at", "link", "proof", "ref", "seq", "type")
	signatureMembers = memberSet("alg", "key_id", "sig")
)

// memberSet builds a set from member names.
func memberSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// checkExactMembers parses the signed bytes as the canonical tree and refuses any object member
// outside the exact set allowed at its position. It runs over the same bytes the signature and the
// leaf recomputation read, so a value a verdict reads is one this has already approved by its exact
// name. Disclosures and attestations are not descended into: this product refuses any bundle that
// carries them, by name and with its own message, so there is nothing of theirs a verdict ever reads.
func checkExactMembers(signed []byte) error {
	value, err := jcs.Parse(signed)
	if err != nil {
		return fmt.Errorf("%w: parse bundle: %w", ErrVerify, err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: bundle is not a JSON object", ErrVerify)
	}
	if err := exactMembers(root, bundleMembers, "bundle"); err != nil {
		return err
	}
	if err := exactChild(root, "producer", producerMembers, "producer"); err != nil {
		return err
	}
	if err := exactChild(root, "subject", subjectMembers, "subject"); err != nil {
		return err
	}
	if chain, ok := root["chain"].(map[string]any); ok {
		if err := exactMembers(chain, chainMembers, "chain"); err != nil {
			return err
		}
		if err := exactChild(chain, "consistency", consistencyMembers, "chain.consistency"); err != nil {
			return err
		}
		if err := exactChild(chain, "head", coordsMembers, "chain.head"); err != nil {
			return err
		}
	}
	if err := exactClaims(root); err != nil {
		return err
	}
	if err := exactArray(root, "signatures", signatureMembers, "signature"); err != nil {
		return err
	}
	return exactArray(root, "anchors", anchorMembers, "anchor")
}

// exactClaims checks every claim object and the sub-objects a verdict reads from it. The payload is
// open and is not descended into; disclosures and attestations are refused elsewhere and are not
// descended into here.
func exactClaims(root map[string]any) error {
	claims, ok := root["claims"].([]any)
	if !ok {
		return nil
	}
	for i, c := range claims {
		obj, ok := c.(map[string]any)
		if !ok {
			continue
		}
		where := fmt.Sprintf("claim %d", i)
		if err := exactMembers(obj, claimMembers, where); err != nil {
			return err
		}
		if err := exactChild(obj, "chain", coordsMembers, where+" chain"); err != nil {
			return err
		}
		if err := exactChild(obj, "inclusion", inclusionMembers, where+" inclusion"); err != nil {
			return err
		}
		if err := exactChild(obj, "verdict", verdictMembers, where+" verdict"); err != nil {
			return err
		}
		if err := exactArray(obj, "evidence", evidenceMembers, where+" evidence"); err != nil {
			return err
		}
	}
	return nil
}

// exactChild checks an optional child object against its member set.
func exactChild(parent map[string]any, key string, allowed map[string]bool, where string) error {
	child, ok := parent[key].(map[string]any)
	if !ok {
		return nil
	}
	return exactMembers(child, allowed, where)
}

// exactArray checks every object in an optional array member against its member set.
func exactArray(parent map[string]any, key string, allowed map[string]bool, where string) error {
	arr, ok := parent[key].([]any)
	if !ok {
		return nil
	}
	for j, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if err := exactMembers(obj, allowed, fmt.Sprintf("%s %d", where, j)); err != nil {
			return err
		}
	}
	return nil
}

// exactMembers refuses the first object member outside the allowed set, naming it. The lowest name is
// reported so the message is stable across runs regardless of map iteration order.
func exactMembers(obj map[string]any, allowed map[string]bool, where string) error {
	var extra []string
	for k := range obj {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	return fmt.Errorf("%w: %s carries an unknown member %q; a member a verdict reads is matched by "+
		"its exact name, so a case variant of a known member is refused rather than folded onto it",
		ErrVerify, where, extra[0])
}
