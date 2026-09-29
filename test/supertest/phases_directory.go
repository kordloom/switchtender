package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// directoryIssuer is the iss claim the install is configured to expect, and the one the harness
// mints for. It is not a URL anything resolves: nothing fetches the issuer for bearer JWT sign-in,
// only the JWKS, and pointing it at a real address would be a dependency for no coverage.
const directoryIssuer = "https://supertest.invalid/directory"

// directoryAudience is the aud claim, set so the install refuses a token minted for somebody else.
const directoryAudience = "switchtender-supertest"

// directoryKey is one RSA key pair, its JWKS entry, and the tokens it can sign.
type directoryKey struct {
	// Private is the signing half, which exists only in this process and only for this run.
	Private *rsa.PrivateKey
	// KeyID names the key inside the JWKS, so a token can say which key signed it.
	KeyID string
}

// newDirectoryKey mints a key pair for a run of the supertest.
func newDirectoryKey(keyID string) (*directoryKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("mint a directory signing key: %w", err)
	}
	return &directoryKey{Private: key, KeyID: keyID}, nil
}

// jwksEntry renders the public half the way a directory publishes it.
func (d *directoryKey) jwksEntry() map[string]any {
	exponent := make([]byte, 4)
	binary.BigEndian.PutUint32(exponent, uint32(d.Private.E))
	return map[string]any{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": d.KeyID,
		"n":   base64.RawURLEncoding.EncodeToString(d.Private.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(trimLeadingZeros(exponent)),
	}
}

// trimLeadingZeros drops the padding in front of a big-endian number, which a JWKS exponent must
// not carry.
func trimLeadingZeros(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

// sign returns a signed RS256 token carrying subject as the identity the install should read.
func (d *directoryKey) sign(subject string, lifetime time.Duration) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": d.KeyID}
	now := time.Now()
	claims := map[string]any{
		"iss": directoryIssuer,
		"aud": directoryAudience,
		"sub": subject,
		"iat": now.Unix(),
		"exp": now.Add(lifetime).Unix(),
	}
	encode := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	head, err := encode(header)
	if err != nil {
		return "", err
	}
	body, err := encode(claims)
	if err != nil {
		return "", err
	}
	signing := head + "." + body
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, d.Private, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign the directory token: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// startDirectory publishes a JWKS the install can fetch and returns its URL.
//
// The key is minted here and the private half never leaves this process, so a run cannot be
// replayed against a later one and nothing on disk is a credential.
func (h *harness) startDirectory(namespace string, key *directoryKey) (string, error) {
	jwks, err := json.Marshal(map[string]any{"keys": []any{key.jwksEntry()}})
	if err != nil {
		return "", fmt.Errorf("render the JWKS: %w", err)
	}
	if _, err := h.kubectl("create", "namespace", namespace); err != nil {
		if out, _ := h.kubectl("get", "namespace", namespace); !strings.Contains(out, namespace) {
			return "", fmt.Errorf("create the %s namespace for the directory: %w", namespace, err)
		}
	}
	path := filepath.Join(h.work, "jwks.json")
	if err := os.WriteFile(path, jwks, 0o600); err != nil {
		return "", fmt.Errorf("write the JWKS: %w", err)
	}
	if _, err := h.kubectl("create", "configmap", "jwksmint", "-n", namespace,
		"--from-file=jwks.json="+path); err != nil {
		return "", fmt.Errorf("publish the JWKS: %w", err)
	}
	manifest := strings.ReplaceAll(mustManifest("jwks.yaml"), "namespace: team",
		"namespace: "+namespace)
	if err := h.apply(manifest); err != nil {
		return "", err
	}
	if _, err := h.kubectl("rollout", "status", "-n", namespace, "deploy/jwksmint",
		"--timeout=120s"); err != nil {
		return "", fmt.Errorf("the directory never became ready: %w", err)
	}
	return fmt.Sprintf("http://jwksmint.%s.svc.cluster.local:8099/jwks.json", namespace), nil
}

// checkDirectorySignInWorks proves an install configured against a directory accepts an identity
// that directory vouched for, and refuses one it did not.
//
// Directory sign-in is the whole of what the Pro tier adds, and no deployed install had ever been
// asked to do it. Whether a running process can fetch a JWKS from an address outside itself and
// then trust a token against it is not a question a unit test settles.
//
// The refusal is the half that makes the acceptance mean anything. An install that accepted a token
// signed by a key it never heard of would accept the good one too, and the check would read exactly
// the same.
func (h *harness) checkDirectorySignInWorks(phase, namespace string) {
	const claim = "an identity the directory vouched for signs in, and one it did not is refused"
	key, err := newDirectoryKey("supertest-directory")
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	jwksURL, err := h.startDirectory(namespace, key)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	// Added to the install that is already running. An install told about a directory at first
	// boot enforces from its first request and deliberately mints no bootstrap token, which is
	// right of the product and leaves this suite with no way in. Adding it afterwards is also what
	// an operator does, since the tier is bought after the install exists.
	if out, err := h.run("helm", "upgrade", namespace,
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", namespace,
		"--kubeconfig", h.kubeconfig,
		"--reuse-values",
		"--set", "server.extraArgs="+jwtServeArgs(jwksURL),
		"--wait", "--timeout", "300s"); err != nil {
		h.fail(phase, claim, fmt.Errorf("point the running install at the directory: %w\n%s",
			err, oneLine(out)))
		return
	}
	// The server rolled, so the forward went with it.
	service, err := h.serviceName(namespace)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	if err := h.forwardTo(namespace, service); err != nil {
		h.fail(phase, claim, err)
		return
	}
	token, err := key.sign("directory-person", time.Hour)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	who := &actor{Name: "directory-person", Type: "user", Token: token}
	var listed map[string]any
	if err := h.apiCall("GET", "/v1/runs", who, nil, &listed); err != nil {
		h.fail(phase, claim, fmt.Errorf("the directory's own identity was refused: %w", err))
		return
	}

	// A key the directory never published. Same issuer, same audience, same subject, same shape:
	// the only difference is who signed it, which is the only thing that should matter.
	stranger, err := newDirectoryKey(key.KeyID)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	forged, err := stranger.sign("directory-person", time.Hour)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	status, body := h.rawGet(&actor{Name: "directory-person", Type: "user", Token: forged},
		"/v1/runs")
	switch {
	case status == 0:
		h.fail(phase, claim, fmt.Errorf("%w: the forged token was never answered, so the refusal "+
			"was not observed: %s", ErrUnreachable, oneLine(body)))
	case status == http.StatusOK:
		h.fail(phase, claim, fmt.Errorf("a token signed by a key this install never fetched was "+
			"accepted, so the directory it checks against decides nothing"))
	case status != http.StatusUnauthorized && status != http.StatusForbidden:
		h.fail(phase, claim, fmt.Errorf("the forged token answered %d, which is a malfunction "+
			"rather than a refusal: %s", status, oneLine(body)))
	default:
		h.pass(phase, claim, fmt.Sprintf("a token signed by the published key read the history, "+
			"and one signed by a key of the same shape answered %d", status))
	}
}

// jwtServeArgs are the flags that point an install at the directory, as helm extraArgs.
func jwtServeArgs(jwksURL string) string {
	return "{--jwt-jwks-url," + jwksURL +
		",--jwt-issuer," + directoryIssuer +
		",--jwt-audience," + directoryAudience + "}"
}
