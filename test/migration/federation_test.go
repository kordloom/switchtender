package migration

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/outcome"
)

// federationIssuer is the issuer URL the federation scenarios serve as. Nothing fetches it: with
// the file delivery the cloud is never contacted, and the scenario verifies the token itself.
const federationIssuer = "https://switchtender.example.invalid"

// updateTemplate rewrites an imported template through the API, changing what mutate changes and
// keeping everything else as the import left it.
func (in *install) updateTemplate(s *server, name string, mutate func(map[string]any)) {
	in.t.Helper()
	id := in.template(s, name)
	var page struct {
		// Templates are the templates.
		Templates []map[string]any `json:"templates"`
	}
	in.must(s, "admin", "GET", "/v1/templates", nil, 200).decode(in.t, &page)
	for _, tpl := range page.Templates {
		if str(tpl["id"]) != id {
			continue
		}
		for _, readOnly := range []string{"id", "created_at", "updated_at", "created_by",
			"callback_key_set", "needs_attention"} {
			delete(tpl, readOnly)
		}
		mutate(tpl)
		in.must(s, "admin", "PUT", "/v1/templates/"+id, tpl, 200)
		return
	}
	in.t.Fatalf("no template %s", id)
}

// createFederated creates an AWS federated credential with a short token lifetime and returns its
// id.
func (in *install) createFederated(s *server, name string) string {
	in.t.Helper()
	var cred struct {
		// ID is the credential's id.
		ID string `json:"id"`
	}
	in.must(s, "admin", "POST", "/v1/credentials", map[string]any{
		"name": name, "kind": "aws_oidc",
		"settings": map[string]any{
			"role_arn":    "arn:aws:iam::123456789012:role/switchtender-deploy",
			"region":      "us-east-1",
			"environment": "prod",
			"token_ttl":   "5m",
		},
	}, 201, 200).decode(in.t, &cred)
	if cred.ID == "" {
		in.t.Fatalf("the federated credential has no id")
	}
	return cred.ID
}

// verifyJWT checks an RS256 token against the issuer's published key set and returns its claims.
func (in *install) verifyJWT(s *server, token string) map[string]any {
	in.t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		in.t.Fatalf("the identity token is not a JWT: %d parts", len(parts))
	}
	var header struct {
		// Alg is the signing algorithm.
		Alg string `json:"alg"`
		// Kid names the signing key.
		Kid string `json:"kid"`
	}
	decodeSegment(in.t, parts[0], &header)
	if header.Alg != "RS256" {
		in.t.Fatalf("the identity token is signed with %s, want RS256", header.Alg)
	}
	var jwks struct {
		// Keys are the published keys.
		Keys []struct {
			// Kid names the key.
			Kid string `json:"kid"`
			// N is the RSA modulus.
			N string `json:"n"`
			// E is the RSA exponent.
			E string `json:"e"`
		} `json:"keys"`
	}
	in.must(s, "", "GET", "/.well-known/jwks.json", nil, 200).decode(in.t, &jwks)
	var pub *rsa.PublicKey
	for _, k := range jwks.Keys {
		if k.Kid != header.Kid {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(k.N)
		e, eerr := base64.RawURLEncoding.DecodeString(k.E)
		if nerr != nil || eerr != nil {
			in.t.Fatalf("the published key %s does not decode", k.Kid)
		}
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if pub == nil {
		in.t.Fatalf("the token's key %s is not in the published key set", header.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		in.t.Fatalf("the token's signature does not decode: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		in.t.Fatalf("the identity token does not verify against the published key: %v", err)
	}
	var claims map[string]any
	decodeSegment(in.t, parts[1], &claims)
	return claims
}

// decodeSegment decodes one base64url JWT segment as JSON.
func decodeSegment(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("a token segment does not decode: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("a token segment is not JSON: %v", err)
	}
}

// tokenKID returns the key id in a token's header.
func tokenKID(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the identity token is not a JWT: %d parts", len(parts))
	}
	var header struct {
		// Kid names the signing key.
		Kid string `json:"kid"`
	}
	decodeSegment(t, parts[0], &header)
	return header.Kid
}

// federatedRun launches the cloud deploy template as the operator, has the approver release it, and
// requires it to succeed. It returns the run's id, the token the play recorded, the path the token
// file had, and when the play read it.
func (in *install) federatedRun(s *server) (string, string, string, time.Time) {
	in.t.Helper()
	rec := in.launched(s, "operator", "cloud deploy", nil)
	in.waitStatus(s, rec.ID, "pending_approval")
	in.must(s, "approver", "POST", "/v1/runs/"+rec.ID+"/approve", nil, 200)
	done := in.waitDone(s, rec.ID)
	if done.Status != "succeeded" {
		in.t.Fatalf("the federated run = %s: %s", done.Status, describe(done.Raw))
	}
	lines := strings.SplitN(in.marker("cloud", "web1"), "\n", 4)
	if len(lines) < 3 {
		in.t.Fatalf("the play recorded no token: %q", lines)
	}
	path, role, token := lines[0], lines[1], strings.TrimSpace(lines[2])
	in.addSecret(token)
	if role != "arn:aws:iam::123456789012:role/switchtender-deploy" {
		in.t.Errorf("the play saw AWS_ROLE_ARN %q", role)
	}
	info, err := os.Stat(filepath.Join(in.markers, "cloud-web1"))
	if err != nil {
		in.t.Fatalf("stat the marker: %v", err)
	}
	requireGone(in.t, "the identity token file", path, filepath.Dir(path))
	return rec.ID, token, path, info.ModTime()
}

// childSetting returns the value the install's processes are given for name.
func (in *install) childSetting(name string) string {
	in.t.Helper()
	for _, kv := range in.env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	in.t.Fatalf("the install sets no %s", name)
	return ""
}

// addKeyMaterial reads every federation signing key that still holds its private half straight
// from the install's database, opens it with the install's encryption key, and adds what a leak
// would carry to the values no answer, log, or record may hold: the PEM, each line of it, and the
// private exponent as a JSON web key would encode it. The database holds the sealed form, so the
// scan of the database file is the proof that it never holds the opened one. It returns the ids of
// the keys it read.
func (in *install) addKeyMaterial() []string {
	in.t.Helper()
	db, err := sql.Open("sqlite", "file:"+in.db+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		in.t.Fatalf("open the database: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT id, sealed FROM federation_keys WHERE sealed <> ''")
	if err != nil {
		in.t.Fatalf("read the signing keys: %v", err)
	}
	defer func() { _ = rows.Close() }()
	sealer := credential.NewSealer(in.childSetting("SWITCHTENDER_ENCRYPTION_KEY"),
		in.childSetting("SWITCHTENDER_ENCRYPTION_SALT"))
	var ids []string
	for rows.Next() {
		var id, sealed string
		if err := rows.Scan(&id, &sealed); err != nil {
			in.t.Fatalf("read a signing key: %v", err)
		}
		plain, err := sealer.Open(sealed)
		if err != nil {
			in.t.Fatalf("open signing key %s: %v", id, err)
		}
		in.addSecret(plain)
		for _, line := range strings.Split(plain, "\n") {
			if len(line) >= 32 && !strings.HasPrefix(line, "-----") {
				in.addSecret(line)
			}
		}
		block, _ := pem.Decode([]byte(plain))
		if block == nil {
			in.t.Fatalf("signing key %s does not open to PEM", id)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			in.t.Fatalf("parse signing key %s: %v", id, err)
		}
		priv, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			in.t.Fatalf("signing key %s is not RSA", id)
		}
		in.addSecret(base64.RawURLEncoding.EncodeToString(priv.D.Bytes()))
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		in.t.Fatalf("read the signing keys: %v", err)
	}
	return ids
}

// publishedKIDs returns the key ids the install publishes.
func (in *install) publishedKIDs(s *server) []string {
	in.t.Helper()
	var jwks struct {
		// Keys are the published keys.
		Keys []struct {
			// Kid names the key.
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	in.must(s, "", "GET", "/.well-known/jwks.json", nil, 200).decode(in.t, &jwks)
	var ids []string
	for _, k := range jwks.Keys {
		ids = append(ids, k.Kid)
	}
	return ids
}

// rotation is the answer to a rotation request.
type rotation struct {
	// Key is the key the rotation created.
	Key struct {
		// ID is the key id.
		ID string `json:"id"`
		// State is pending, signing, retired, or removed.
		State string `json:"state"`
		// ActivatedAt is when the key starts signing.
		ActivatedAt time.Time `json:"activated_at"`
	} `json:"key"`
}

// requireIssuance fails the scenario unless the run's receipt discloses exactly one token issuance,
// naming the run, the credential, the key that signed token, and token's own id.
func requireIssuance(t *testing.T, rec *receipt, credID, token string) {
	t.Helper()
	var found []outcome.TokenIssuance
	for _, c := range rec.Claims {
		if c.Method != "TOKEN" {
			continue
		}
		ti, ok := outcome.ParseTokenPath(c.Path)
		if !ok {
			t.Errorf("the receipt of %s discloses a token entry that does not read: %s", rec.RunID,
				c.Path)
			continue
		}
		if ti.RunID == rec.RunID {
			found = append(found, ti)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the receipt of %s discloses %d token issuances for it, want 1", rec.RunID, len(found))
	}
	var claims struct {
		// ID is the token's jti.
		ID string `json:"jti"`
	}
	decodeSegment(t, strings.Split(token, ".")[1], &claims)
	if found[0].CredentialID != credID || found[0].KeyID != tokenKID(t, token) ||
		found[0].TokenID != claims.ID {
		t.Errorf("the receipt of %s records %+v, want %s, the token's kid %s, and its jti %s",
			rec.RunID, found[0], credID, tokenKID(t, token), claims.ID)
	}
}

// TestFederatedAWSCredentialTokenIsScopedAndLeavesNoTrace is scenario nine. An imported template is
// given a federated AWS credential with the file delivery. While the run executes, the token file
// has to exist and hold a token the issuer's published key verifies, valid no longer than the
// credential's lifetime, naming the run, the template, and the person who approved it. Once the run
// ends the file has to be gone, and the token must appear in no record.
//
// The signing key's lifecycle runs on the real server too. A normal rotation publishes a second key
// and the next run is still signed by the first, a second rotation is refused while the first is
// under way, and an emergency rotation leaves only its own key published and signs the run after
// it. Each run's receipt names the key that signed its token, and no signing key's private half
// appears in any answer, server log, or the database file.
func TestFederatedAWSCredentialTokenIsScopedAndLeavesNoTrace(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite, Policy: holdAnsible})
	s := in.startServer("a", "--federation-issuer", federationIssuer)
	credID := in.createFederated(s, "aws-prod-deploy")
	in.updateTemplate(s, "cloud deploy", func(tpl map[string]any) {
		tpl["credential_ids"] = []string{credID}
	})

	first, token, _, readAt := in.federatedRun(s)
	claims := in.verifyJWT(s, token)
	num := func(name string) time.Time {
		f, ok := claims[name].(float64)
		if !ok {
			t.Fatalf("the token has no numeric %s claim: %v", name, claims)
		}
		return time.Unix(int64(f), 0)
	}
	iat, exp := num("iat"), num("exp")
	if lifetime := exp.Sub(iat); lifetime <= 0 || lifetime > 5*time.Minute {
		t.Errorf("the token lives %s, want no longer than the credential's 5m", lifetime)
	}
	if !readAt.Before(exp) || readAt.Before(iat.Add(-time.Minute)) {
		t.Errorf("the play read the token at %s, outside its validity %s to %s", readAt, iat, exp)
	}
	for name, want := range map[string]any{
		"iss": federationIssuer, "aud": "sts.amazonaws.com", "run_id": first,
		"template_id": in.template(s, "cloud deploy"), "credential_id": credID,
		"environment": "prod", "approved": true, "approved_by": "approver-laptop",
		"run_type": "apply", "tool": "ansible", "actor": "operator-laptop",
	} {
		if claims[name] != want {
			t.Errorf("the token's %s claim = %v, want %v", name, claims[name], want)
		}
	}
	if sub := str(claims["sub"]); !strings.Contains(sub, ":approved:true") ||
		!strings.Contains(sub, ":env:prod:") {
		t.Errorf("the token's subject %q does not carry the environment and the approval", sub)
	}
	original := tokenKID(t, token)
	if got := in.addKeyMaterial(); len(got) != 1 || got[0] != original {
		t.Fatalf("signing keys before any rotation = %v, want the one that signed, %s", got, original)
	}

	var normal rotation
	in.must(s, "admin", "POST", "/v1/federation/keys/rotate", nil, 200).decode(t, &normal)
	if normal.Key.State != "pending" || time.Until(normal.Key.ActivatedAt) < 23*time.Hour {
		t.Errorf("a normal rotation answered %+v, want a key pending for a day", normal.Key)
	}
	if got := in.publishedKIDs(s); len(got) != 2 {
		t.Errorf("published after a normal rotation = %v, want the signing key and the pending one", got)
	}
	in.must(s, "admin", "POST", "/v1/federation/keys/rotate", nil, 409)
	in.addKeyMaterial()
	second, secondToken, _, _ := in.federatedRun(s)
	in.verifyJWT(s, secondToken)
	if got := tokenKID(t, secondToken); got != original {
		t.Errorf("the run after a normal rotation was signed by %s, want the key still signing, %s",
			got, original)
	}

	var emergency rotation
	in.must(s, "admin", "POST", "/v1/federation/keys/rotate/emergency", nil, 200).decode(t, &emergency)
	if emergency.Key.State != "signing" {
		t.Errorf("an emergency rotation answered %+v, want a key signing at once", emergency.Key)
	}
	if got := in.publishedKIDs(s); len(got) != 1 || got[0] != emergency.Key.ID {
		t.Errorf("published after an emergency rotation = %v, want only %s", got, emergency.Key.ID)
	}
	if got := in.addKeyMaterial(); len(got) != 1 || got[0] != emergency.Key.ID {
		t.Errorf("keys holding a private half after an emergency rotation = %v, want only %s", got,
			emergency.Key.ID)
	}
	third, thirdToken, _, _ := in.federatedRun(s)
	in.verifyJWT(s, thirdToken)
	if got := tokenKID(t, thirdToken); got != emergency.Key.ID {
		t.Errorf("the run after an emergency rotation was signed by %s, want %s", got, emergency.Key.ID)
	}
	if dirs := in.runFileDirs(); len(dirs) != 0 {
		t.Errorf("run directories left after the federated runs: %v", dirs)
	}

	ev := in.checkEvidence(s, first, second, third)
	requireRecord(t, ev.Receipts[first], recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Approver: "approver-laptop",
		Playbook: "cloud.yml", Hosts: []string{"web1"}, CredentialIDs: []string{credID},
		Rules: []string{"ansible-waits-for-a-person"},
	})
	for id, tok := range map[string]string{first: token, second: secondToken, third: thirdToken} {
		requireIssuance(t, ev.Receipts[id], credID, tok)
	}
	if len(ev.entries("/v1/federation/keys/rotate/emergency")) != 1 {
		t.Error("the audit trail does not record the emergency rotation under its own path")
	}
}
