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

// stubRuleEvaluator is the Community rule evaluator: it refuses naming the tier the criteria need,
// Team when any Team criterion is set and Pro otherwise, so the caller holds the run and says why.
type stubRuleEvaluator struct{}

// Matches refuses.
func (stubRuleEvaluator) Matches(_ *run.Run, c RuleCriteria) (bool, error) {
	if c.RequireDistinctApprover || c.Actor != "" || c.Account != "" || c.ActorKind != "" {
		return false, Requires(license.FeaturePolicyFull)
	}
	return false, Requires(license.FeaturePolicyRules)
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

// stubWorker is the Community worker: it refuses naming Team before touching anything.
type stubWorker struct{}

// Run refuses.
func (stubWorker) Run(context.Context, WorkerConfig) error {
	return Requires(license.FeatureWorkers)
}

// stubReconciler is the Community reconciler: it refuses naming Team.
type stubReconciler struct{}

// Propose refuses.
func (stubReconciler) Propose(context.Context, ReconcileRequest) ([]run.SubmitOption, error) {
	return nil, Requires(license.FeatureReconcile)
}
