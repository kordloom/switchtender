package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/federation"
)

// federationIssuerEnv names the environment variable that supplies --federation-issuer, so serve
// and every worker on one database can read one value from a shared environment file.
const federationIssuerEnv = "SWITCHTENDER_FEDERATION_ISSUER"

// federationIssuer holds the --federation-issuer flag: the external URL this install is an OpenID
// Connect issuer at. Empty leaves workload identity federation off.
var federationIssuer string

// registerFederationFlag adds the --federation-issuer flag, shared by serve and worker. Both need
// it: serve publishes the keys at that URL, and whichever process executes a run signs its token
// with that URL as the issuer, so the two must agree.
func registerFederationFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&federationIssuer, "federation-issuer", os.Getenv(federationIssuerEnv),
		"External https URL this install serves as an OpenID Connect issuer, so runs reach clouds "+
			"with short-lived federated credentials. Empty leaves federation off.")
}

// newFederationIssuer builds the issuer at rawURL, the --federation-issuer value, with its signing
// keys in keys and sealed by sealer, recording every token it mints in audits. It returns nil when
// the URL is empty, and an error when the URL is unusable, when there is no encryption key to seal
// the signing key with, or when there is no audit chain to record which key signed each token, so a
// process asked to federate never starts without being able to do all three.
func newFederationIssuer(rawURL string, keys federation.KeyStore, sealer *credential.Sealer,
	audits audit.Store) (*federation.Issuer, error) {
	if rawURL == "" {
		return nil, nil
	}
	if audits == nil {
		return nil, fmt.Errorf("%w: each token's signing key is recorded in the audit chain, and "+
			"this process has none", federation.ErrIssuer)
	}
	return federation.NewIssuer(rawURL, keys, sealer,
		federation.WithIssuanceRecorder(dispatch.TokenEvidence(audits)))
}
