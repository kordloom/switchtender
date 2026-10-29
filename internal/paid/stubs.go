package paid

import (
	"context"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// stubDirectory is the Community directory sign-in: every kind refuses naming Pro.
type stubDirectory struct{}

// OIDC refuses.
func (stubDirectory) OIDC(context.Context, OIDCConfig) (OIDCDirectory, error) {
	return nil, Requires(license.FeatureSSO)
}

// SAML refuses.
func (stubDirectory) SAML(context.Context, SAMLConfig) (SAMLDirectory, error) {
	return nil, Requires(license.FeatureSSO)
}

// LDAP refuses.
func (stubDirectory) LDAP(context.Context, LDAPConfig) (LDAPDirectory, error) {
	return nil, Requires(license.FeatureSSO)
}

// JWT refuses.
func (stubDirectory) JWT(context.Context, JWTConfig) (JWTDirectory, error) {
	return nil, Requires(license.FeatureSSO)
}

// stubRegoCompiler is the Community Rego compiler: it refuses naming Team.
type stubRegoCompiler struct{}

// Compile refuses.
func (stubRegoCompiler) Compile([]RegoModule, RegoSettings) (RegoProgram, error) {
	return nil, Requires(license.FeaturePolicyFull)
}

// stubChangeRegister is the Community change register: it refuses naming Team.
type stubChangeRegister struct{}

// NewEmitter refuses.
func (stubChangeRegister) NewEmitter(EmitterConfig) (Emitter, error) {
	return nil, Requires(license.FeatureRegister)
}

// Render refuses.
func (stubChangeRegister) Render(context.Context, RegisterSource, RegisterPeriod) ([]byte, error) {
	return nil, Requires(license.FeatureRegister)
}

// Build refuses.
func (stubChangeRegister) Build(context.Context, RegisterSource, RegisterPeriod) (any, error) {
	return nil, Requires(license.FeatureRegister)
}

// stubReconciler is the Community reconciler: it refuses naming Team.
type stubReconciler struct{}

// Propose refuses.
func (stubReconciler) Propose(context.Context, ReconcileRequest) ([]run.SubmitOption, error) {
	return nil, Requires(license.FeatureReconcile)
}
