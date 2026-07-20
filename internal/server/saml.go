package server

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/auth"
	"github.com/dcadolph/switchtender/internal/user"
)

// samlStateTTL bounds how long a sign-in may take from the redirect to the assertion post.
const samlStateTTL = 10 * time.Minute

// samlCookie names the short-lived signed cookie that carries the request id across the redirect,
// so the assertion's InResponseTo is checked against the request this browser actually started.
const samlCookie = "ym_saml"

// SAMLAuth signs users in through a SAML identity provider. SwitchTender is the service provider: it
// redirects the browser to the IdP with an authentication request and consumes the signed assertion
// at the ACS endpoint. It provisions a local account on first sign-in and mints the same session
// token the password login does, so a SAML user is treated like any other.
type SAMLAuth struct {
	// sp holds the service provider keys, endpoints, and the IdP metadata.
	sp saml.ServiceProvider
	// users provisions and looks up accounts.
	users user.Store
	// tokens persists the minted session token.
	tokens auth.Store
	// defaultRole is granted when no asserted group maps to a role.
	defaultRole user.Role
	// roleMap maps a lowercased group attribute value to a role, so the IdP drives a user's role on
	// every sign-in. Empty leaves everyone on the default role.
	roleMap map[string]user.Role
	// usernameAttr is the assertion attribute used as the username. Empty uses the subject NameID.
	usernameAttr string
	// groupsAttr is the assertion attribute holding the user's groups for role mapping.
	groupsAttr string
	// signKey signs the request-id cookie. It is derived from the service provider key so every
	// server instance signs alike without shared session state.
	signKey []byte
	// secureCookie marks the request-id cookie secure, set when the base URL is https.
	secureCookie bool
	// log records sign-in activity without assertion contents.
	log *zap.Logger
}

// NewSAMLAuth fetches the IdP metadata and builds a SAMLAuth. The certificate and key files hold
// the service provider's PEM keypair. Metadata discovery does network I/O, so this runs at startup
// and a failure stops the server rather than half-enabling sign-in.
func NewSAMLAuth(ctx context.Context, idpMetadataURL, baseURL, certFile, keyFile,
	usernameAttr, groupsAttr string, defaultRole user.Role, roleMap map[string]user.Role,
	users user.Store, tokens auth.Store, log *zap.Logger) (*SAMLAuth, error) {
	if idpMetadataURL == "" || baseURL == "" || certFile == "" || keyFile == "" {
		return nil, errors.New("saml: idp metadata url, base url, certificate, and key are required")
	}
	if !user.ValidRole(defaultRole) {
		return nil, fmt.Errorf("saml: invalid default role %q", defaultRole)
	}
	keyPair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("saml: load keypair: %w", err)
	}
	cert, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("saml: parse certificate: %w", err)
	}
	key, ok := keyPair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("saml: the service provider key must be RSA")
	}
	mdURL, err := url.Parse(idpMetadataURL)
	if err != nil {
		return nil, fmt.Errorf("saml: idp metadata url: %w", err)
	}
	idpMetadata, err := samlsp.FetchMetadata(ctx, http.DefaultClient, *mdURL)
	if err != nil {
		return nil, fmt.Errorf("saml: fetch idp metadata: %w", err)
	}
	base := strings.TrimRight(baseURL, "/")
	acsURL, err := url.Parse(base + "/auth/saml/acs")
	if err != nil {
		return nil, fmt.Errorf("saml: base url: %w", err)
	}
	metadataURL, _ := url.Parse(base + "/auth/saml/metadata")
	sigKey := sha256.Sum256(append([]byte("switchtender-saml\x00"), x509.MarshalPKCS1PrivateKey(key)...))
	return &SAMLAuth{
		sp: saml.ServiceProvider{
			EntityID:    base + "/auth/saml/metadata",
			Key:         key,
			Certificate: cert,
			AcsURL:      *acsURL,
			MetadataURL: *metadataURL,
			IDPMetadata: idpMetadata,
		},
		users: users, tokens: tokens,
		defaultRole: defaultRole, roleMap: roleMap,
		usernameAttr: usernameAttr, groupsAttr: groupsAttr,
		signKey:      sigKey[:],
		secureCookie: strings.HasPrefix(strings.ToLower(base), "https"),
		log:          log,
	}, nil
}

// login starts sign-in: it builds an authentication request, remembers its id in a signed
// short-lived cookie, and redirects the browser to the identity provider.
func (s *SAMLAuth) login(w http.ResponseWriter, r *http.Request) {
	req, err := s.sp.MakeAuthenticationRequest(
		s.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding),
		saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		s.log.Error("saml: make request: " + err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	target, err := req.Redirect("", &s.sp)
	if err != nil {
		s.log.Error("saml: build redirect: " + err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	s.setRequestID(w, req.ID)
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// acs consumes the identity provider's response: it validates the assertion against the request id
// from the signed cookie, provisions or finds the account, mints a session token, and hands it to
// the browser in the redirect fragment where the UI stores it.
func (s *SAMLAuth) acs(w http.ResponseWriter, r *http.Request) {
	reqID, err := s.readRequestID(r)
	s.clearRequestID(w)
	if err != nil {
		s.fail(w, r, "sign-in expired, try again")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "sign-in response unreadable")
		return
	}
	assertion, err := s.sp.ParseResponse(r, []string{reqID})
	if err != nil {
		s.log.Warn("saml: assertion rejected: " + err.Error())
		s.fail(w, r, "sign-in failed")
		return
	}
	username := s.username(assertion)
	if username == "" {
		s.fail(w, r, "sign-in returned no username")
		return
	}
	role := roleForGroups(s.groups(assertion), s.roleMap, s.defaultRole)
	u, err := provisionFromDirectory(r.Context(), s.users, s.log, username, role,
		len(s.roleMap) > 0, "saml")
	if err != nil {
		s.log.Error("saml: provision account: " + err.Error())
		s.fail(w, r, "could not sign in")
		return
	}
	plain, err := mintSessionToken(r.Context(), s.tokens, u)
	if err != nil {
		s.log.Error("saml: mint session: " + err.Error())
		s.fail(w, r, "could not sign in")
		return
	}
	frag := url.Values{"access_token": {plain}, "role": {string(u.Role)}, "user": {u.Username}}
	http.Redirect(w, r, "/ui/#"+frag.Encode(), http.StatusFound)
}

// metadata serves the service provider metadata XML an IdP administrator uses to register
// SwitchTender: the entity id, the ACS endpoint, and the signing certificate.
func (s *SAMLAuth) metadata(w http.ResponseWriter, _ *http.Request) {
	out, err := xml.MarshalIndent(s.sp.Metadata(), "", "  ")
	if err != nil {
		s.log.Error("saml: marshal metadata: " + err.Error())
		http.Error(w, "metadata unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write(out)
}

// username returns the asserted username: the configured attribute when set, else the subject
// NameID.
func (s *SAMLAuth) username(a *saml.Assertion) string {
	if s.usernameAttr != "" {
		if vs := attributeValues(a, s.usernameAttr); len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if a.Subject != nil && a.Subject.NameID != nil {
		return a.Subject.NameID.Value
	}
	return ""
}

// groups returns the asserted group values used for role mapping.
func (s *SAMLAuth) groups(a *saml.Assertion) []string {
	return attributeValues(a, s.groupsAttr)
}

// attributeValues collects every value of the named assertion attribute, matching the attribute
// name or friendly name case-insensitively so OneLogin, Okta, and ADFS naming all work.
func attributeValues(a *saml.Assertion, name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	for _, stmt := range a.AttributeStatements {
		for _, attr := range stmt.Attributes {
			if !strings.EqualFold(attr.Name, name) && !strings.EqualFold(attr.FriendlyName, name) {
				continue
			}
			for _, v := range attr.Values {
				if v.Value != "" {
					out = append(out, v.Value)
				}
			}
		}
	}
	return out
}

// fail sends the browser back to the sign-in page with a message it can show.
func (s *SAMLAuth) fail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/ui/login#"+url.Values{"error": {msg}}.Encode(), http.StatusFound)
}

// setRequestID writes the signed, short-lived cookie carrying the authentication request id.
func (s *SAMLAuth) setRequestID(w http.ResponseWriter, id string) {
	exp := strconv.FormatInt(time.Now().Add(samlStateTTL).Unix(), 10)
	payload := id + "|" + exp
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.sign([]byte(payload))
	http.SetCookie(w, &http.Cookie{
		Name: samlCookie, Value: value, Path: "/auth/saml", HttpOnly: true,
		Secure: s.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(samlStateTTL.Seconds()),
	})
}

// readRequestID verifies the cookie signature and expiry and returns the request id it carries.
func (s *SAMLAuth) readRequestID(r *http.Request) (string, error) {
	c, err := r.Cookie(samlCookie)
	if err != nil {
		return "", err
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", errors.New("saml: malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return "", errors.New("saml: bad state signature")
	}
	id, expStr, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", errors.New("saml: malformed state payload")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", errors.New("saml: state expired")
	}
	return id, nil
}

// clearRequestID expires the request-id cookie once the ACS has read it.
func (s *SAMLAuth) clearRequestID(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: samlCookie, Path: "/auth/saml", MaxAge: -1})
}

// sign returns the base64 HMAC-SHA256 of payload under the derived key.
func (s *SAMLAuth) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.signKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
