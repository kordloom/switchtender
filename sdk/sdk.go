// Package sdk is the stable surface a SwitchTender extension builds against. It re-exports the
// extension interfaces and their registration entry points, so an extension imports this one
// package and never reaches into SwitchTender's internals.
//
// Register from an init function or from main before the server starts. The registries are read
// while serving and never written after, so registration is a startup step, not a runtime one. A
// bad registration, such as a duplicate or reserved name, panics, which surfaces the mistake at
// startup rather than on first use.
//
// Registering a tool from an init function:
//
//	func init() {
//		sdk.RegisterTool("hello", sdk.ToolRunnerFunc(
//			func(ctx context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
//				fmt.Fprintln(out, spec.Command)
//				return sdk.ToolResult{ExitCode: 0}, nil
//			}))
//	}
package sdk

import (
	"context"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// AIProvider turns a prompt into a completion for an advisory feature. It never sits in the
// execution path. Register one with RegisterAIProvider.
type AIProvider = ai.Provider

// AIProviderFunc adapts a plain function to an AIProvider.
type AIProviderFunc = ai.ProviderFunc

// AIProviderFactory builds an AIProvider from its settings: the model, the endpoint URL, and an API
// key. A factory validates its own required settings, so a missing key is an error at startup.
type AIProviderFactory = ai.Factory

// RegisterAIProvider adds a model backend under name, such as a local or hosted API. It panics on an
// empty or duplicate name.
func RegisterAIProvider(name string, factory AIProviderFactory) {
	ai.Register(name, factory)
}

// AIProviderNames returns the registered AI provider names, sorted.
func AIProviderNames() []string {
	return ai.Names()
}

// ToolRunner executes one run of a tool, streaming output to a writer and reporting the result.
type ToolRunner = roundhouse.Runner

// ToolRunnerFunc adapts a plain function to a ToolRunner.
type ToolRunnerFunc = roundhouse.RunnerFunc

// ToolSpec is the input to a tool run: the tool name, its command, extra vars, and dry-run flag.
type ToolSpec = roundhouse.Spec

// ToolResult is the outcome of a tool run, carrying the process exit code.
type ToolResult = roundhouse.Result

// RegisterTool adds an execution tool in one step: name makes a run with that tool valid to submit,
// and runner executes it. A registered tool takes its input from a run's Command, like every
// non-Ansible built-in. It panics on a nil runner or an empty, duplicate, or built-in name.
func RegisterTool(name string, runner ToolRunner) {
	if runner == nil {
		panic("sdk: nil runner for tool " + name)
	}
	run.RegisterTool(name)
	roundhouse.RegisterRunner(name, runner)
}

// Run is a single execution, passed to a Notifier when it reaches a terminal state.
type Run = run.Run

// Notifier delivers a terminal top-level run to an external channel. The run is redacted of extra
// vars first, so a notifier never receives values that can carry secrets.
type Notifier = dispatch.Notifier

// NotifierFunc adapts a plain function to a Notifier.
type NotifierFunc = dispatch.NotifierFunc

// RegisterNotifier adds a notification channel under name, beside the built-in webhook, Slack, and
// email delivery. It panics on an empty or duplicate name or a nil notifier.
func RegisterNotifier(name string, n Notifier) {
	dispatch.RegisterNotifier(name, n)
}

// SecretResolver fetches a value from a source's config at run time, so a new secrets engine backs
// any resource, not just a credential.
type SecretResolver = secretsource.ResolverFunc

// RegisterSecretSource adds a secrets engine under kind, such as AWS Secrets Manager or 1Password.
// It panics on an empty, duplicate, or reserved kind.
func RegisterSecretSource(kind string, resolver SecretResolver) {
	secretsource.Register(kind, resolver)
}

// SecretLease is a handle to a minted short-lived secret. Revoking it ends the secret early.
type SecretLease = secretsource.Lease

// NewSecretLease builds the lease a SecretMinter returns, naming the engine that minted the secret
// and capturing how to revoke it. Pass a nil revoke func for a secret that only expires on the
// engine's own TTL.
func NewSecretLease(kind string, revoke func(context.Context) error) *SecretLease {
	return secretsource.NewLease(kind, revoke)
}

// SecretMinter mints a short-lived value from a dynamic engine's config, returning the value and a
// lease that revokes it after the run.
type SecretMinter = secretsource.MintFunc

// RegisterDynamicSecretSource adds a dynamic secrets engine under kind, such as AWS STS, that mints
// a short-lived credential on each read. It panics on an empty, duplicate, or reserved kind.
func RegisterDynamicSecretSource(kind string, minter SecretMinter) {
	secretsource.RegisterDynamic(kind, minter)
}

// Beat is one span beat as the feed at BeatFeedPath serves it. It is the wire contract an
// out-of-tree witness builds against: the server produces this shape and a watcher consumes it, so
// exposing it here keeps an external watcher from guessing the fields and drifting from the server.
type Beat = beatfeed.Beat

// BeatFeedPath is the request path the span beat feed is served at, version prefix included, so an
// external witness fetches <base>+BeatFeedPath. The feed is unauthenticated by design: the party it
// exists to convince has no account on the watched server.
const BeatFeedPath = beatfeed.APIPath

// BeatFeedLimitParam is the query parameter that bounds how many beats one feed request returns.
const BeatFeedLimitParam = beatfeed.LimitParam
