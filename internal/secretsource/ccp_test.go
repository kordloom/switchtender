package secretsource

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestResolveCCP exercises the CyberArk CCP resolver against a mock AIMWebService, covering the
// account lookup, its query parameters, a custom web service id, and the error cases.
func TestResolveCCP(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		switch gotQuery.Get("Object") {
		case "empty":
			_, _ = w.Write([]byte(`{"Content":""}`))
		case "garbage":
			_, _ = w.Write([]byte(`not json`))
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"Content":"s3cr3t","UserName":"svc"}`))
		}
	}))
	defer srv.Close()

	cfg := func(c ccpConfig) string {
		c.URL = srv.URL
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: An account located by safe, folder, and object returns its Content, and every locator
	// is sent as a query parameter to the default AIMWebService path.
	got, err := resolveCCP(context.Background(), cfg(ccpConfig{
		AppID: "app1", Safe: "Prod", Folder: "Root", Object: "db-prod", Reason: "deploy",
	}))
	if err != nil || got != "s3cr3t" {
		t.Errorf("lookup = %q, %v; want s3cr3t", got, err)
	}
	if gotPath != "/AIMWebService/api/Accounts" {
		t.Errorf("path = %q, want the default AIMWebService path", gotPath)
	}
	for k, want := range map[string]string{
		"AppID": "app1", "Safe": "Prod", "Folder": "Root", "Object": "db-prod", "Reason": "deploy",
	} {
		if gotQuery.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, gotQuery.Get(k), want)
		}
	}

	// Test 1: A raw query locates the account instead of safe and object.
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{
		AppID: "app1", Query: "Safe=Prod;Object=db-prod",
	})); err != nil {
		t.Errorf("query lookup err = %v", err)
	}
	if gotQuery.Get("Query") != "Safe=Prod;Object=db-prod" {
		t.Errorf("Query param = %q, want the raw query", gotQuery.Get("Query"))
	}

	// Test 2: A custom web service id changes the request path.
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{
		AppID: "app1", Object: "db-prod", WebServiceID: "AIMWebServiceX",
	})); err != nil {
		t.Errorf("custom web service err = %v", err)
	}
	if gotPath != "/AIMWebServiceX/api/Accounts" {
		t.Errorf("custom path = %q, want AIMWebServiceX", gotPath)
	}

	// Test 3: A missing url, app id, or account locator errors before any request.
	if _, err := resolveCCP(context.Background(), `{"app_id":"a","object":"o"}`); !errors.Is(err, ErrResolve) {
		t.Errorf("missing url error = %v, want ErrResolve", err)
	}
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{Object: "db-prod"})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing app_id error = %v, want ErrResolve", err)
	}
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{AppID: "app1"})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing locator error = %v, want ErrResolve", err)
	}

	// Test 4: An empty Content, a non-JSON response, an unknown object, and bad config all error.
	for name, c := range map[string]ccpConfig{
		"empty content": {AppID: "app1", Object: "empty"},
		"bad response":  {AppID: "app1", Object: "garbage"},
		"unknown":       {AppID: "app1", Object: "missing"},
	} {
		if _, err := resolveCCP(context.Background(), cfg(c)); !errors.Is(err, ErrResolve) {
			t.Errorf("%s error = %v, want ErrResolve", name, err)
		}
	}
	if _, err := resolveCCP(context.Background(), "{bad"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}

	// Test 5: An invalid client certificate or CA certificate errors before any request.
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{
		AppID: "app1", Object: "db-prod", ClientCert: "not a pem", ClientKey: "nope",
	})); !errors.Is(err, ErrResolve) {
		t.Errorf("bad client cert error = %v, want ErrResolve", err)
	}
	if _, err := resolveCCP(context.Background(), cfg(ccpConfig{
		AppID: "app1", Object: "db-prod", CACert: "not a pem",
	})); !errors.Is(err, ErrResolve) {
		t.Errorf("bad ca cert error = %v, want ErrResolve", err)
	}
}

// TestResolveCCPMutualTLS proves the resolver presents its client certificate to a CCP that asks for
// one and trusts the server through the configured CA, the standard secure CCP setup.
func TestResolveCCPMutualTLS(t *testing.T) {
	t.Parallel()
	clientCertPEM, clientKeyPEM := generateClientCert(t, "switchtender-app")

	var sawClientCert bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClientCert = r.TLS != nil && len(r.TLS.PeerCertificates) > 0
		_, _ = w.Write([]byte(`{"Content":"mtls-secret"}`))
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()

	serverCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	b, _ := json.Marshal(ccpConfig{
		URL: srv.URL, AppID: "app1", Object: "db-prod",
		ClientCert: string(clientCertPEM), ClientKey: string(clientKeyPEM), CACert: string(serverCAPEM),
	})

	got, err := resolveCCP(context.Background(), string(b))
	if err != nil || got != "mtls-secret" {
		t.Fatalf("mutual TLS resolve = %q, %v; want mtls-secret", got, err)
	}
	if !sawClientCert {
		t.Error("the CCP server did not receive the client certificate")
	}
}

// generateClientCert returns a self-signed client certificate and key in PEM for a test identity.
func generateClientCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
